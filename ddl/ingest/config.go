// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ingest

import (
	"path/filepath"
	"sync/atomic"

	"github.com/pingcap/tidb/br/pkg/lightning/backend"
	"github.com/pingcap/tidb/br/pkg/lightning/checkpoints"
	"github.com/pingcap/tidb/br/pkg/lightning/config"
	tidbconf "github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/size"
	"go.uber.org/zap"
)

// ImporterRangeConcurrencyForTest is only used for test.
var ImporterRangeConcurrencyForTest *atomic.Int32

func generateLightningConfig(memRoot MemRoot, jobID int64, unique bool) (*config.Config, error) {
	tidbCfg := tidbconf.GetGlobalConfig()
	cfg := config.NewConfig()
	cfg.TikvImporter.Backend = config.BackendLocal
	// Each backend will build a single dir in lightning dir.
	cfg.TikvImporter.SortedKVDir = filepath.Join(LitSortPath, encodeBackendTag(jobID))
	if ImporterRangeConcurrencyForTest != nil {
		cfg.TikvImporter.RangeConcurrency = int(ImporterRangeConcurrencyForTest.Load())
	}
	_, err := cfg.AdjustCommon()
	if err != nil {
		logutil.BgLogger().Warn(LitWarnConfigError, zap.Error(err))
		return nil, err
	}
	adjustImportMemory(memRoot, cfg)
	cfg.Checkpoint.Enable = true
	if unique {
		cfg.TikvImporter.DuplicateResolution = config.DupeResAlgErr
	} else {
		cfg.TikvImporter.DuplicateResolution = config.DupeResAlgNone
	}
	cfg.TiDB.PdAddr = tidbCfg.Path
	cfg.TiDB.Host = "127.0.0.1"
	cfg.TiDB.StatusPort = int(tidbCfg.Status.StatusPort)
	// Set TLS related information
	cfg.Security.CAPath = tidbCfg.Security.ClusterSSLCA
	cfg.Security.CertPath = tidbCfg.Security.ClusterSSLCert
	cfg.Security.KeyPath = tidbCfg.Security.ClusterSSLKey

	return cfg, err
}

var (
	compactMemory      = 1 * size.GB
	compactConcurrency = 4
)

func generateLocalEngineConfig(id int64, dbName, tbName string) *backend.EngineConfig {
	return &backend.EngineConfig{
		Local: &backend.LocalEngineConfig{
			Compact:            true,
			CompactThreshold:   int64(compactMemory),
			CompactConcurrency: compactConcurrency,
		},
		TableInfo: &checkpoints.TidbTableInfo{
			ID:   id,
			DB:   dbName,
			Name: tbName,
		},
	}
}

// adjustImportMemory adjusts the lightning memory parameters according to the memory root's max limitation.
func adjustImportMemory(memRoot MemRoot, cfg *config.Config) {
	var scale int64
	// Try aggressive resource usage successful.
	if tryAggressiveMemory(memRoot, cfg) {
		return
	}

	defaultMemSize := int64(cfg.TikvImporter.LocalWriterMemCacheSize) * int64(cfg.TikvImporter.RangeConcurrency)
	defaultMemSize += 4 * int64(cfg.TikvImporter.EngineMemCacheSize)
	logutil.BgLogger().Info(LitInfoInitMemSetting,
		zap.Int64("local writer memory cache size", int64(cfg.TikvImporter.LocalWriterMemCacheSize)),
		zap.Int64("engine memory cache size", int64(cfg.TikvImporter.EngineMemCacheSize)),
		zap.Int("range concurrency", cfg.TikvImporter.RangeConcurrency))

	maxLimit := memRoot.MaxMemoryQuota()
	scale = defaultMemSize / maxLimit

	if scale == 1 || scale == 0 {
		return
	}

	cfg.TikvImporter.LocalWriterMemCacheSize /= config.ByteSize(scale)
	cfg.TikvImporter.EngineMemCacheSize /= config.ByteSize(scale)
	// TODO: adjust range concurrency number to control total concurrency in the future.
	logutil.BgLogger().Info(LitInfoChgMemSetting,
		zap.Int64("local writer memory cache size", int64(cfg.TikvImporter.LocalWriterMemCacheSize)),
		zap.Int64("engine memory cache size", int64(cfg.TikvImporter.EngineMemCacheSize)),
		zap.Int("range concurrency", cfg.TikvImporter.RangeConcurrency))
}

// tryAggressiveMemory lightning memory parameters according memory root's max limitation.
func tryAggressiveMemory(memRoot MemRoot, cfg *config.Config) bool {
	var defaultMemSize int64
	defaultMemSize = int64(int(cfg.TikvImporter.LocalWriterMemCacheSize) * cfg.TikvImporter.RangeConcurrency)
	defaultMemSize += int64(cfg.TikvImporter.EngineMemCacheSize)

	if (defaultMemSize + memRoot.CurrentUsage()) > memRoot.MaxMemoryQuota() {
		return false
	}
	logutil.BgLogger().Info(LitInfoChgMemSetting,
		zap.Int64("local writer memory cache size", int64(cfg.TikvImporter.LocalWriterMemCacheSize)),
		zap.Int64("engine memory cache size", int64(cfg.TikvImporter.EngineMemCacheSize)),
		zap.Int("range concurrency", cfg.TikvImporter.RangeConcurrency))
	return true
}

// defaultImportantVariables is used in obtainImportantVariables to retrieve the system
// variables from downstream which may affect KV encode result. The values record the default
// values if missing.
var defaultImportantVariables = map[string]string{
	"max_allowed_packet":      "67108864", // 64MB
	"div_precision_increment": "4",
	"time_zone":               "SYSTEM",
	"lc_time_names":           "en_US",
	"default_week_format":     "0",
	"block_encryption_mode":   "aes-128-ecb",
	"group_concat_max_len":    "1024",
	"tidb_row_format_version": "1",
}
