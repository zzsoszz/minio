/*
 * MinIO Cloud Storage, (C) 2020 MinIO, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"net/http"
	"sync"
	"time"

	"github.com/minio/minio/cmd/config/api"
	"github.com/minio/minio/cmd/logger"
	"github.com/minio/minio/pkg/sys"
)

type apiConfig struct {
	mu sync.RWMutex

	requestsDeadline time.Duration
	requestsPool     chan struct{}
	clusterDeadline  time.Duration
	listQuorum       int
	extendListLife   time.Duration
	corsAllowOrigins []string
	setDriveCount    int
}

func (t *apiConfig) init(cfg api.Config, setDriveCount int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.clusterDeadline = cfg.ClusterDeadline
	t.corsAllowOrigins = cfg.CorsAllowOrigin
	t.setDriveCount = setDriveCount

	var apiRequestsMaxPerNode int
	if cfg.RequestsMax <= 0 {
		stats, err := sys.GetStats()
		if err != nil {
			logger.LogIf(GlobalContext, err)
			// Default to 16 GiB, not critical.
			stats.TotalRAM = 16 << 30
		}
		// max requests per node is calculated as
		// total_ram / ram_per_request
		// ram_per_request is 4MiB * setDriveCount + 2 * 10MiB (default erasure block size)
		apiRequestsMaxPerNode = int(stats.TotalRAM / uint64(setDriveCount*(writeBlockSize+readBlockSize)+blockSizeV1*2))
	} else {
		apiRequestsMaxPerNode = cfg.RequestsMax
		if len(globalEndpoints.Hostnames()) > 0 {
			apiRequestsMaxPerNode /= len(globalEndpoints.Hostnames())
		}
	}
	if cap(t.requestsPool) < apiRequestsMaxPerNode {
		// Only replace if needed.
		// Existing requests will use the previous limit,
		// but new requests will use the new limit.
		// There will be a short overlap window,
		// but this shouldn't last long.
		t.requestsPool = make(chan struct{}, apiRequestsMaxPerNode)
	}
	t.requestsDeadline = cfg.RequestsDeadline
	t.listQuorum = cfg.GetListQuorum()
	t.extendListLife = cfg.ExtendListLife
}

func (t *apiConfig) getListQuorum() int {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.listQuorum
}

func (t *apiConfig) getSetDriveCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.setDriveCount
}

func (t *apiConfig) getExtendListLife() time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.extendListLife
}

func (t *apiConfig) getCorsAllowOrigins() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	corsAllowOrigins := make([]string, len(t.corsAllowOrigins))
	copy(corsAllowOrigins, t.corsAllowOrigins)
	return corsAllowOrigins
}

func (t *apiConfig) getClusterDeadline() time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.clusterDeadline == 0 {
		return 10 * time.Second
	}

	return t.clusterDeadline
}

func (t *apiConfig) getRequestsPool() (chan struct{}, time.Duration) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.requestsPool == nil {
		return nil, time.Duration(0)
	}

	return t.requestsPool, t.requestsDeadline
}

// maxClients throttles the S3 API calls
func maxClients(f http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pool, deadline := globalAPIConfig.getRequestsPool()
		if pool == nil {
			f.ServeHTTP(w, r)
			return
		}

		deadlineTimer := time.NewTimer(deadline)
		defer deadlineTimer.Stop()

		select {
		case pool <- struct{}{}:
			defer func() { <-pool }()
			f.ServeHTTP(w, r)
		case <-deadlineTimer.C:
			// Send a http timeout message
			writeErrorResponse(r.Context(), w,
				errorCodes.ToAPIErr(ErrOperationMaxedOut),
				r.URL, guessIsBrowserReq(r))
			return
		case <-r.Context().Done():
			return
		}
	}
}
