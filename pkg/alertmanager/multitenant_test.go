package alertmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/http/pprof"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/prometheus/alertmanager/cluster/clusterpb"
	"github.com/prometheus/alertmanager/pkg/labels"
	"github.com/prometheus/alertmanager/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thanos-io/thanos/pkg/objstore"
	"github.com/weaveworks/common/httpgrpc"
	"github.com/weaveworks/common/user"
	"google.golang.org/grpc"

	"github.com/cortexproject/cortex/pkg/alertmanager/alertmanagerpb"
	"github.com/cortexproject/cortex/pkg/alertmanager/alertspb"
	"github.com/cortexproject/cortex/pkg/alertmanager/alertstore"
	"github.com/cortexproject/cortex/pkg/alertmanager/alertstore/bucketclient"
	"github.com/cortexproject/cortex/pkg/ring"
	"github.com/cortexproject/cortex/pkg/ring/kv/consul"
	"github.com/cortexproject/cortex/pkg/storage/bucket"
	"github.com/cortexproject/cortex/pkg/util"
	"github.com/cortexproject/cortex/pkg/util/flagext"
	"github.com/cortexproject/cortex/pkg/util/services"
	"github.com/cortexproject/cortex/pkg/util/test"
)

var (
	simpleConfigOne = `route:
  receiver: dummy

receivers:
  - name: dummy`

	simpleConfigTwo = `route:
  receiver: dummy

receivers:
  - name: dummy`
)

func mockAlertmanagerConfig(t *testing.T) *MultitenantAlertmanagerConfig {
	t.Helper()

	externalURL := flagext.URLValue{}
	err := externalURL.Set("http://localhost/api/prom")
	require.NoError(t, err)

	tempDir, err := ioutil.TempDir(os.TempDir(), "alertmanager")
	require.NoError(t, err)

	t.Cleanup(func() {
		err := os.RemoveAll(tempDir)
		require.NoError(t, err)
	})

	cfg := &MultitenantAlertmanagerConfig{}
	flagext.DefaultValues(cfg)

	cfg.ExternalURL = externalURL
	cfg.DataDir = tempDir
	cfg.ShardingRing.InstanceID = "test"
	cfg.ShardingRing.InstanceAddr = "127.0.0.1"
	cfg.PollInterval = time.Minute

	return cfg
}

func TestMultitenantAlertmanagerConfig_Validate(t *testing.T) {
	// Default values only.
	{
		cfg := &MultitenantAlertmanagerConfig{}
		flagext.DefaultValues(cfg)
		assert.NoError(t, cfg.Validate())
	}
	// Invalid persist interval (zero).
	{
		cfg := &MultitenantAlertmanagerConfig{}
		flagext.DefaultValues(cfg)
		cfg.Persister.Interval = 0
		assert.Equal(t, errInvalidPersistInterval, cfg.Validate())
	}
	// Invalid persist interval (negative).
	{
		cfg := &MultitenantAlertmanagerConfig{}
		flagext.DefaultValues(cfg)
		cfg.Persister.Interval = -1
		assert.Equal(t, errInvalidPersistInterval, cfg.Validate())
	}
}

func TestMultitenantAlertmanager_loadAndSyncConfigs(t *testing.T) {
	ctx := context.Background()

	// Run this test using a real storage client.
	store := prepareInMemoryAlertStore()
	require.NoError(t, store.SetAlertConfig(ctx, alertspb.AlertConfigDesc{
		User:      "user1",
		RawConfig: simpleConfigOne,
		Templates: []*alertspb.TemplateDesc{},
	}))
	require.NoError(t, store.SetAlertConfig(ctx, alertspb.AlertConfigDesc{
		User:      "user2",
		RawConfig: simpleConfigOne,
		Templates: []*alertspb.TemplateDesc{},
	}))

	reg := prometheus.NewPedanticRegistry()
	cfg := mockAlertmanagerConfig(t)
	am, err := createMultitenantAlertmanager(cfg, nil, nil, store, nil, log.NewNopLogger(), reg)
	require.NoError(t, err)

	// Ensure the configs are synced correctly
	err = am.loadAndSyncConfigs(context.Background(), reasonPeriodic)
	require.NoError(t, err)
	require.Len(t, am.alertmanagers, 2)

	currentConfig, cfgExists := am.cfgs["user1"]
	require.True(t, cfgExists)
	require.Equal(t, simpleConfigOne, currentConfig.RawConfig)

	assert.NoError(t, testutil.GatherAndCompare(reg, bytes.NewBufferString(`
		# HELP cortex_alertmanager_config_last_reload_successful Boolean set to 1 whenever the last configuration reload attempt was successful.
		# TYPE cortex_alertmanager_config_last_reload_successful gauge
		cortex_alertmanager_config_last_reload_successful{user="user1"} 1
		cortex_alertmanager_config_last_reload_successful{user="user2"} 1
	`), "cortex_alertmanager_config_last_reload_successful"))

	// Ensure when a 3rd config is added, it is synced correctly
	user3Cfg := alertspb.AlertConfigDesc{
		User: "user3",
		RawConfig: simpleConfigOne + `
templates:
- 'first.tpl'
- 'second.tpl'
`,
		Templates: []*alertspb.TemplateDesc{
			{
				Filename: "first.tpl",
				Body:     `{{ define "t1" }}Template 1 ... {{end}}`,
			},
			{
				Filename: "second.tpl",
				Body:     `{{ define "t2" }}Template 2{{ end}}`,
			},
		},
	}
	require.NoError(t, store.SetAlertConfig(ctx, user3Cfg))

	err = am.loadAndSyncConfigs(context.Background(), reasonPeriodic)
	require.NoError(t, err)
	require.Len(t, am.alertmanagers, 3)

	dirs := am.getPerUserDirectories()
	user3Dir := dirs["user3"]
	require.NotZero(t, user3Dir)
	require.True(t, dirExists(t, user3Dir))
	require.True(t, dirExists(t, filepath.Join(user3Dir, templatesDir)))
	require.True(t, fileExists(t, filepath.Join(user3Dir, templatesDir, "first.tpl")))
	require.True(t, fileExists(t, filepath.Join(user3Dir, templatesDir, "second.tpl")))

	assert.NoError(t, testutil.GatherAndCompare(reg, bytes.NewBufferString(`
		# HELP cortex_alertmanager_config_last_reload_successful Boolean set to 1 whenever the last configuration reload attempt was successful.
		# TYPE cortex_alertmanager_config_last_reload_successful gauge
		cortex_alertmanager_config_last_reload_successful{user="user1"} 1
		cortex_alertmanager_config_last_reload_successful{user="user2"} 1
		cortex_alertmanager_config_last_reload_successful{user="user3"} 1
	`), "cortex_alertmanager_config_last_reload_successful"))

	// Ensure the config is updated
	require.NoError(t, store.SetAlertConfig(ctx, alertspb.AlertConfigDesc{
		User:      "user1",
		RawConfig: simpleConfigTwo,
		Templates: []*alertspb.TemplateDesc{},
	}))

	err = am.loadAndSyncConfigs(context.Background(), reasonPeriodic)
	require.NoError(t, err)

	currentConfig, cfgExists = am.cfgs["user1"]
	require.True(t, cfgExists)
	require.Equal(t, simpleConfigTwo, currentConfig.RawConfig)

	// Test Delete User, ensure config is removed and the resources are freed.
	require.NoError(t, store.DeleteAlertConfig(ctx, "user3"))
	err = am.loadAndSyncConfigs(context.Background(), reasonPeriodic)
	require.NoError(t, err)
	currentConfig, cfgExists = am.cfgs["user3"]
	require.False(t, cfgExists)
	require.Equal(t, "", currentConfig.RawConfig)

	_, cfgExists = am.alertmanagers["user3"]
	require.False(t, cfgExists)
	dirs = am.getPerUserDirectories()
	require.NotZero(t, dirs["user1"])
	require.NotZero(t, dirs["user2"])
	require.Zero(t, dirs["user3"]) // User3 is deleted, so we should have no more files for it.
	require.False(t, fileExists(t, user3Dir))

	assert.NoError(t, testutil.GatherAndCompare(reg, bytes.NewBufferString(`
		# HELP cortex_alertmanager_config_last_reload_successful Boolean set to 1 whenever the last configuration reload attempt was successful.
		# TYPE cortex_alertmanager_config_last_reload_successful gauge
		cortex_alertmanager_config_last_reload_successful{user="user1"} 1
		cortex_alertmanager_config_last_reload_successful{user="user2"} 1
	`), "cortex_alertmanager_config_last_reload_successful"))

	// Ensure when a 3rd config is re-added, it is synced correctly
	require.NoError(t, store.SetAlertConfig(ctx, user3Cfg))

	err = am.loadAndSyncConfigs(context.Background(), reasonPeriodic)
	require.NoError(t, err)

	currentConfig, cfgExists = am.cfgs["user3"]
	require.True(t, cfgExists)
	require.Equal(t, user3Cfg.RawConfig, currentConfig.RawConfig)

	_, cfgExists = am.alertmanagers["user3"]
	require.True(t, cfgExists)
	dirs = am.getPerUserDirectories()
	require.NotZero(t, dirs["user1"])
	require.NotZero(t, dirs["user2"])
	require.Equal(t, user3Dir, dirs["user3"]) // Dir should exist, even though state files are not generated yet.

	// Hierarchy that existed before should exist again.
	require.True(t, dirExists(t, user3Dir))
	require.True(t, dirExists(t, filepath.Join(user3Dir, templatesDir)))
	require.True(t, fileExists(t, filepath.Join(user3Dir, templatesDir, "first.tpl")))
	require.True(t, fileExists(t, filepath.Join(user3Dir, templatesDir, "second.tpl")))

	assert.NoError(t, testutil.GatherAndCompare(reg, bytes.NewBufferString(`
		# HELP cortex_alertmanager_config_last_reload_successful Boolean set to 1 whenever the last configuration reload attempt was successful.
		# TYPE cortex_alertmanager_config_last_reload_successful gauge
		cortex_alertmanager_config_last_reload_successful{user="user1"} 1
		cortex_alertmanager_config_last_reload_successful{user="user2"} 1
		cortex_alertmanager_config_last_reload_successful{user="user3"} 1
	`), "cortex_alertmanager_config_last_reload_successful"))
}

func TestMultitenantAlertmanager_migrateStateFilesToPerTenantDirectories(t *testing.T) {
	ctx := context.Background()

	const (
		user1 = "user1"
		user2 = "user2"
	)

	store := prepareInMemoryAlertStore()
	require.NoError(t, store.SetAlertConfig(ctx, alertspb.AlertConfigDesc{
		User:      user2,
		RawConfig: simpleConfigOne,
		Templates: []*alertspb.TemplateDesc{},
	}))

	reg := prometheus.NewPedanticRegistry()
	cfg := mockAlertmanagerConfig(t)
	am, err := createMultitenantAlertmanager(cfg, nil, nil, store, nil, log.NewNopLogger(), reg)
	require.NoError(t, err)

	createFile(t, filepath.Join(cfg.DataDir, "nflog:"+user1))
	createFile(t, filepath.Join(cfg.DataDir, "silences:"+user1))
	createFile(t, filepath.Join(cfg.DataDir, "nflog:"+user2))
	createFile(t, filepath.Join(cfg.DataDir, "templates", user2, "template.tpl"))

	require.NoError(t, am.migrateStateFilesToPerTenantDirectories())
	require.True(t, fileExists(t, filepath.Join(cfg.DataDir, user1, notificationLogSnapshot)))
	require.True(t, fileExists(t, filepath.Join(cfg.DataDir, user1, silencesSnapshot)))
	require.True(t, fileExists(t, filepath.Join(cfg.DataDir, user2, notificationLogSnapshot)))
	require.True(t, dirExists(t, filepath.Join(cfg.DataDir, user2, templatesDir)))
	require.True(t, fileExists(t, filepath.Join(cfg.DataDir, user2, templatesDir, "template.tpl")))
}

func fileExists(t *testing.T, path string) bool {
	return checkExists(t, path, false)
}

func dirExists(t *testing.T, path string) bool {
	return checkExists(t, path, true)
}

func checkExists(t *testing.T, path string, dir bool) bool {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		require.NoError(t, err)
	}

	require.Equal(t, dir, fi.IsDir())
	return true
}

func TestMultitenantAlertmanager_deleteUnusedLocalUserState(t *testing.T) {
	ctx := context.Background()

	const (
		user1 = "user1"
		user2 = "user2"
	)

	store := prepareInMemoryAlertStore()
	require.NoError(t, store.SetAlertConfig(ctx, alertspb.AlertConfigDesc{
		User:      user2,
		RawConfig: simpleConfigOne,
		Templates: []*alertspb.TemplateDesc{},
	}))

	reg := prometheus.NewPedanticRegistry()
	cfg := mockAlertmanagerConfig(t)
	am, err := createMultitenantAlertmanager(cfg, nil, nil, store, nil, log.NewNopLogger(), reg)
	require.NoError(t, err)

	createFile(t, filepath.Join(cfg.DataDir, user1, notificationLogSnapshot))
	createFile(t, filepath.Join(cfg.DataDir, user1, silencesSnapshot))
	createFile(t, filepath.Join(cfg.DataDir, user2, notificationLogSnapshot))
	createFile(t, filepath.Join(cfg.DataDir, user2, templatesDir, "template.tpl"))

	dirs := am.getPerUserDirectories()
	require.Equal(t, 2, len(dirs))
	require.NotZero(t, dirs[user1])
	require.NotZero(t, dirs[user2])

	// Ensure the configs are synced correctly
	err = am.loadAndSyncConfigs(context.Background(), reasonPeriodic)
	require.NoError(t, err)

	// loadAndSyncConfigs also cleans up obsolete files. Let's verify that.
	dirs = am.getPerUserDirectories()

	require.Zero(t, dirs[user1])    // has no configuration, files were deleted
	require.NotZero(t, dirs[user2]) // has config, files survived
}

func createFile(t *testing.T, path string) string {
	dir := filepath.Dir(path)
	require.NoError(t, os.MkdirAll(dir, 0777))
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return path
}

func TestMultitenantAlertmanager_NoExternalURL(t *testing.T) {
	amConfig := mockAlertmanagerConfig(t)
	amConfig.ExternalURL = flagext.URLValue{} // no external URL

	// Create the Multitenant Alertmanager.
	reg := prometheus.NewPedanticRegistry()
	_, err := NewMultitenantAlertmanager(amConfig, nil, log.NewNopLogger(), reg)

	require.EqualError(t, err, "unable to create Alertmanager because the external URL has not been configured")
}

func TestMultitenantAlertmanager_ServeHTTP(t *testing.T) {
	// Run this test using a real storage client.
	store := prepareInMemoryAlertStore()

	amConfig := mockAlertmanagerConfig(t)

	externalURL := flagext.URLValue{}
	err := externalURL.Set("http://localhost:8080/alertmanager")
	require.NoError(t, err)

	amConfig.ExternalURL = externalURL

	// Create the Multitenant Alertmanager.
	reg := prometheus.NewPedanticRegistry()
	am, err := createMultitenantAlertmanager(amConfig, nil, nil, store, nil, log.NewNopLogger(), reg)
	require.NoError(t, err)

	require.NoError(t, services.StartAndAwaitRunning(context.Background(), am))
	defer services.StopAndAwaitTerminated(context.Background(), am) //nolint:errcheck

	// Request when no user configuration is present.
	req := httptest.NewRequest("GET", externalURL.String(), nil)
	ctx := user.InjectOrgID(req.Context(), "user1")

	{
		w := httptest.NewRecorder()
		am.ServeHTTP(w, req.WithContext(ctx))

		resp := w.Result()
		body, _ := ioutil.ReadAll(resp.Body)
		require.Equal(t, 404, w.Code)
		require.Equal(t, "the Alertmanager is not configured\n", string(body))
	}

	// Create a configuration for the user in storage.
	require.NoError(t, store.SetAlertConfig(ctx, alertspb.AlertConfigDesc{
		User:      "user1",
		RawConfig: simpleConfigTwo,
		Templates: []*alertspb.TemplateDesc{},
	}))

	// Make the alertmanager pick it up.
	err = am.loadAndSyncConfigs(context.Background(), reasonPeriodic)
	require.NoError(t, err)

	// Request when AM is active.
	{
		w := httptest.NewRecorder()
		am.ServeHTTP(w, req.WithContext(ctx))

		require.Equal(t, 301, w.Code) // redirect to UI
	}

	// Verify that GET /metrics returns 404 even when AM is active.
	{
		metricURL := externalURL.String() + "/metrics"
		require.Equal(t, "http://localhost:8080/alertmanager/metrics", metricURL)
		verify404(ctx, t, am, "GET", metricURL)
	}

	// Verify that POST /-/reload returns 404 even when AM is active.
	{
		metricURL := externalURL.String() + "/-/reload"
		require.Equal(t, "http://localhost:8080/alertmanager/-/reload", metricURL)
		verify404(ctx, t, am, "POST", metricURL)
	}

	// Verify that GET /debug/index returns 404 even when AM is active.
	{
		// Register pprof Index (under non-standard path, but this path is exposed by AM using default MUX!)
		http.HandleFunc("/alertmanager/debug/index", pprof.Index)

		metricURL := externalURL.String() + "/debug/index"
		require.Equal(t, "http://localhost:8080/alertmanager/debug/index", metricURL)
		verify404(ctx, t, am, "GET", metricURL)
	}

	// Remove the tenant's Alertmanager
	require.NoError(t, store.DeleteAlertConfig(ctx, "user1"))
	err = am.loadAndSyncConfigs(context.Background(), reasonPeriodic)
	require.NoError(t, err)

	{
		// Request when the alertmanager is gone
		w := httptest.NewRecorder()
		am.ServeHTTP(w, req.WithContext(ctx))

		resp := w.Result()
		body, _ := ioutil.ReadAll(resp.Body)
		require.Equal(t, 404, w.Code)
		require.Equal(t, "the Alertmanager is not configured\n", string(body))
	}
}

func verify404(ctx context.Context, t *testing.T, am *MultitenantAlertmanager, method string, url string) {
	metricsReq := httptest.NewRequest(method, url, strings.NewReader("Hello")) // Body for POST Request.
	w := httptest.NewRecorder()
	am.ServeHTTP(w, metricsReq.WithContext(ctx))

	require.Equal(t, 404, w.Code)
}

func TestMultitenantAlertmanager_ServeHTTPWithFallbackConfig(t *testing.T) {
	ctx := context.Background()
	amConfig := mockAlertmanagerConfig(t)

	// Run this test using a real storage client.
	store := prepareInMemoryAlertStore()

	externalURL := flagext.URLValue{}
	err := externalURL.Set("http://localhost:8080/alertmanager")
	require.NoError(t, err)

	fallbackCfg := `
global:
  smtp_smarthost: 'localhost:25'
  smtp_from: 'youraddress@example.org'
route:
  receiver: example-email
receivers:
  - name: example-email
    email_configs:
    - to: 'youraddress@example.org'
`
	amConfig.ExternalURL = externalURL

	// Create the Multitenant Alertmanager.
	am, err := createMultitenantAlertmanager(amConfig, nil, nil, store, nil, log.NewNopLogger(), nil)
	require.NoError(t, err)
	am.fallbackConfig = fallbackCfg

	require.NoError(t, services.StartAndAwaitRunning(ctx, am))
	defer services.StopAndAwaitTerminated(ctx, am) //nolint:errcheck

	// Request when no user configuration is present.
	req := httptest.NewRequest("GET", externalURL.String()+"/api/v1/status", nil)
	w := httptest.NewRecorder()

	am.ServeHTTP(w, req.WithContext(user.InjectOrgID(req.Context(), "user1")))

	resp := w.Result()

	// It succeeds and the Alertmanager is started.
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Len(t, am.alertmanagers, 1)
	_, exists := am.alertmanagers["user1"]
	require.True(t, exists)

	// Even after a poll...
	err = am.loadAndSyncConfigs(ctx, reasonPeriodic)
	require.NoError(t, err)

	//  It does not remove the Alertmanager.
	require.Len(t, am.alertmanagers, 1)
	_, exists = am.alertmanagers["user1"]
	require.True(t, exists)

	// Remove the Alertmanager configuration.
	require.NoError(t, store.DeleteAlertConfig(ctx, "user1"))
	err = am.loadAndSyncConfigs(ctx, reasonPeriodic)
	require.NoError(t, err)

	// Even after removing it.. We start it again with the fallback configuration.
	w = httptest.NewRecorder()
	am.ServeHTTP(w, req.WithContext(user.InjectOrgID(req.Context(), "user1")))

	resp = w.Result()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestMultitenantAlertmanager_InitialSyncWithSharding(t *testing.T) {
	tc := []struct {
		name          string
		existing      bool
		initialState  ring.InstanceState
		initialTokens ring.Tokens
	}{
		{
			name:     "with no instance in the ring",
			existing: false,
		},
		{
			name:          "with an instance already in the ring with PENDING state and no tokens",
			existing:      true,
			initialState:  ring.PENDING,
			initialTokens: ring.Tokens{},
		},
		{
			name:          "with an instance already in the ring with JOINING state and some tokens",
			existing:      true,
			initialState:  ring.JOINING,
			initialTokens: ring.Tokens{1, 2, 3, 4, 5, 6, 7, 8, 9},
		},
		{
			name:          "with an instance already in the ring with ACTIVE state and all tokens",
			existing:      true,
			initialState:  ring.ACTIVE,
			initialTokens: ring.GenerateTokens(128, nil),
		},
		{
			name:          "with an instance already in the ring with LEAVING state and all tokens",
			existing:      true,
			initialState:  ring.LEAVING,
			initialTokens: ring.Tokens{100000},
		},
	}

	for _, tt := range tc {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			amConfig := mockAlertmanagerConfig(t)
			amConfig.ShardingEnabled = true
			ringStore := consul.NewInMemoryClient(ring.GetCodec())

			// Use an alert store with a mocked backend.
			bkt := &bucket.ClientMock{}
			alertStore := bucketclient.NewBucketAlertStore(bkt, nil, log.NewNopLogger())

			// Setup the initial instance state in the ring.
			if tt.existing {
				require.NoError(t, ringStore.CAS(ctx, RingKey, func(in interface{}) (interface{}, bool, error) {
					ringDesc := ring.GetOrCreateRingDesc(in)
					ringDesc.AddIngester(amConfig.ShardingRing.InstanceID, amConfig.ShardingRing.InstanceAddr, "", tt.initialTokens, tt.initialState, time.Now())
					return ringDesc, true, nil
				}))
			}

			am, err := createMultitenantAlertmanager(amConfig, nil, nil, alertStore, ringStore, log.NewNopLogger(), nil)
			require.NoError(t, err)
			defer services.StopAndAwaitTerminated(ctx, am) //nolint:errcheck

			// Before being registered in the ring.
			require.False(t, am.ringLifecycler.IsRegistered())
			require.Equal(t, ring.PENDING.String(), am.ringLifecycler.GetState().String())
			require.Equal(t, 0, len(am.ringLifecycler.GetTokens()))
			require.Equal(t, ring.Tokens{}, am.ringLifecycler.GetTokens())

			// During the initial sync, we expect two things. That the instance is already
			// registered with the ring (meaning we have tokens) and that its state is JOINING.
			bkt.MockIterWithCallback("alerts/", nil, nil, func() {
				require.True(t, am.ringLifecycler.IsRegistered())
				require.Equal(t, ring.JOINING.String(), am.ringLifecycler.GetState().String())
			})

			// Once successfully started, the instance should be ACTIVE in the ring.
			require.NoError(t, services.StartAndAwaitRunning(ctx, am))

			// After being registered in the ring.
			require.True(t, am.ringLifecycler.IsRegistered())
			require.Equal(t, ring.ACTIVE.String(), am.ringLifecycler.GetState().String())
			require.Equal(t, 128, len(am.ringLifecycler.GetTokens()))
			require.Subset(t, am.ringLifecycler.GetTokens(), tt.initialTokens)
		})
	}
}

func TestMultitenantAlertmanager_PerTenantSharding(t *testing.T) {
	tc := []struct {
		name              string
		tenantShardSize   int
		replicationFactor int
		instances         int
		configs           int
		expectedTenants   int
		withSharding      bool
	}{
		{
			name:            "sharding disabled, 1 instance",
			instances:       1,
			configs:         10,
			expectedTenants: 10,
		},
		{
			name:            "sharding disabled, 2 instances",
			instances:       2,
			configs:         10,
			expectedTenants: 10 * 2, // each instance loads _all_ tenants.
		},
		{
			name:              "sharding enabled, 1 instance, RF = 1",
			withSharding:      true,
			instances:         1,
			replicationFactor: 1,
			configs:           10,
			expectedTenants:   10, // same as no sharding and 1 instance
		},
		{
			name:              "sharding enabled, 2 instances, RF = 1",
			withSharding:      true,
			instances:         2,
			replicationFactor: 1,
			configs:           10,
			expectedTenants:   10, // configs * replication factor
		},
		{
			name:              "sharding enabled, 3 instances, RF = 2",
			withSharding:      true,
			instances:         3,
			replicationFactor: 2,
			configs:           10,
			expectedTenants:   20, // configs * replication factor
		},
		{
			name:              "sharding enabled, 5 instances, RF = 3",
			withSharding:      true,
			instances:         5,
			replicationFactor: 3,
			configs:           10,
			expectedTenants:   30, // configs * replication factor
		},
	}

	for _, tt := range tc {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ringStore := consul.NewInMemoryClient(ring.GetCodec())
			alertStore := prepareInMemoryAlertStore()

			var instances []*MultitenantAlertmanager
			var instanceIDs []string
			registries := util.NewUserRegistries()

			// First, add the number of configs to the store.
			for i := 1; i <= tt.configs; i++ {
				u := fmt.Sprintf("u-%d", i)
				require.NoError(t, alertStore.SetAlertConfig(context.Background(), alertspb.AlertConfigDesc{
					User:      u,
					RawConfig: simpleConfigOne,
					Templates: []*alertspb.TemplateDesc{},
				}))
			}

			// Then, create the alertmanager instances, start them and add their registries to the slice.
			for i := 1; i <= tt.instances; i++ {
				instanceIDs = append(instanceIDs, fmt.Sprintf("alertmanager-%d", i))
				instanceID := fmt.Sprintf("alertmanager-%d", i)

				amConfig := mockAlertmanagerConfig(t)
				amConfig.ShardingRing.ReplicationFactor = tt.replicationFactor
				amConfig.ShardingRing.InstanceID = instanceID
				amConfig.ShardingRing.InstanceAddr = fmt.Sprintf("127.0.0.%d", i)
				// Do not check the ring topology changes or poll in an interval in this test (we explicitly sync alertmanagers).
				amConfig.PollInterval = time.Hour
				amConfig.ShardingRing.RingCheckPeriod = time.Hour

				if tt.withSharding {
					amConfig.ShardingEnabled = true
				}

				reg := prometheus.NewPedanticRegistry()
				am, err := createMultitenantAlertmanager(amConfig, nil, nil, alertStore, ringStore, log.NewNopLogger(), reg)
				require.NoError(t, err)
				defer services.StopAndAwaitTerminated(ctx, am) //nolint:errcheck

				require.NoError(t, services.StartAndAwaitRunning(ctx, am))

				instances = append(instances, am)
				instanceIDs = append(instanceIDs, instanceID)
				registries.AddUserRegistry(instanceID, reg)
			}

			// If we're testing sharding, we need make sure the ring is settled.
			if tt.withSharding {
				ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()

				// The alertmanager is ready to be tested once all instances are ACTIVE and the ring settles.
				for _, am := range instances {
					for _, id := range instanceIDs {
						require.NoError(t, ring.WaitInstanceState(ctx, am.ring, id, ring.ACTIVE))
					}
				}
			}

			// Now that the ring has settled, sync configs with the instances.
			var numConfigs, numInstances int
			for _, am := range instances {
				err := am.loadAndSyncConfigs(ctx, reasonRingChange)
				require.NoError(t, err)
				numConfigs += len(am.cfgs)
				numInstances += len(am.alertmanagers)
			}

			metrics := registries.BuildMetricFamiliesPerUser()
			assert.Equal(t, tt.expectedTenants, numConfigs)
			assert.Equal(t, tt.expectedTenants, numInstances)
			assert.Equal(t, float64(tt.expectedTenants), metrics.GetSumOfGauges("cortex_alertmanager_tenants_owned"))
			assert.Equal(t, float64(tt.configs*tt.instances), metrics.GetSumOfGauges("cortex_alertmanager_tenants_discovered"))
		})
	}
}

func TestMultitenantAlertmanager_SyncOnRingTopologyChanges(t *testing.T) {
	registeredAt := time.Now()

	tc := []struct {
		name       string
		setupRing  func(desc *ring.Desc)
		updateRing func(desc *ring.Desc)
		expected   bool
	}{
		{
			name: "when an instance is added to the ring",
			setupRing: func(desc *ring.Desc) {
				desc.AddIngester("alertmanager-1", "127.0.0.1", "", ring.Tokens{1, 2, 3}, ring.ACTIVE, registeredAt)
			},
			updateRing: func(desc *ring.Desc) {
				desc.AddIngester("alertmanager-2", "127.0.0.2", "", ring.Tokens{4, 5, 6}, ring.ACTIVE, registeredAt)
			},
			expected: true,
		},
		{
			name: "when an instance is removed from the ring",
			setupRing: func(desc *ring.Desc) {
				desc.AddIngester("alertmanager-1", "127.0.0.1", "", ring.Tokens{1, 2, 3}, ring.ACTIVE, registeredAt)
				desc.AddIngester("alertmanager-2", "127.0.0.2", "", ring.Tokens{4, 5, 6}, ring.ACTIVE, registeredAt)
			},
			updateRing: func(desc *ring.Desc) {
				desc.RemoveIngester("alertmanager-1")
			},
			expected: true,
		},
		{
			name: "should sync when an instance changes state",
			setupRing: func(desc *ring.Desc) {
				desc.AddIngester("alertmanager-1", "127.0.0.1", "", ring.Tokens{1, 2, 3}, ring.ACTIVE, registeredAt)
				desc.AddIngester("alertmanager-2", "127.0.0.2", "", ring.Tokens{4, 5, 6}, ring.JOINING, registeredAt)
			},
			updateRing: func(desc *ring.Desc) {
				instance := desc.Ingesters["alertmanager-2"]
				instance.State = ring.ACTIVE
				desc.Ingesters["alertmanager-2"] = instance
			},
			expected: true,
		},
		{
			name: "should sync when an healthy instance becomes unhealthy",
			setupRing: func(desc *ring.Desc) {
				desc.AddIngester("alertmanager-1", "127.0.0.1", "", ring.Tokens{1, 2, 3}, ring.ACTIVE, registeredAt)
				desc.AddIngester("alertmanager-2", "127.0.0.2", "", ring.Tokens{4, 5, 6}, ring.ACTIVE, registeredAt)
			},
			updateRing: func(desc *ring.Desc) {
				instance := desc.Ingesters["alertmanager-1"]
				instance.Timestamp = time.Now().Add(-time.Hour).Unix()
				desc.Ingesters["alertmanager-1"] = instance
			},
			expected: true,
		},
		{
			name: "should sync when an unhealthy instance becomes healthy",
			setupRing: func(desc *ring.Desc) {
				desc.AddIngester("alertmanager-1", "127.0.0.1", "", ring.Tokens{1, 2, 3}, ring.ACTIVE, registeredAt)

				instance := desc.AddIngester("alertmanager-2", "127.0.0.2", "", ring.Tokens{4, 5, 6}, ring.ACTIVE, registeredAt)
				instance.Timestamp = time.Now().Add(-time.Hour).Unix()
				desc.Ingesters["alertmanager-2"] = instance
			},
			updateRing: func(desc *ring.Desc) {
				instance := desc.Ingesters["alertmanager-2"]
				instance.Timestamp = time.Now().Unix()
				desc.Ingesters["alertmanager-2"] = instance
			},
			expected: true,
		},
		{
			name: "should NOT sync when an instance updates the heartbeat",
			setupRing: func(desc *ring.Desc) {
				desc.AddIngester("alertmanager-1", "127.0.0.1", "", ring.Tokens{1, 2, 3}, ring.ACTIVE, registeredAt)
				desc.AddIngester("alertmanager-2", "127.0.0.2", "", ring.Tokens{4, 5, 6}, ring.ACTIVE, registeredAt)
			},
			updateRing: func(desc *ring.Desc) {
				instance := desc.Ingesters["alertmanager-1"]
				instance.Timestamp = time.Now().Add(time.Second).Unix()
				desc.Ingesters["alertmanager-1"] = instance
			},
			expected: false,
		},
		{
			name: "should NOT sync when an instance is auto-forgotten in the ring but was already unhealthy in the previous state",
			setupRing: func(desc *ring.Desc) {
				desc.AddIngester("alertmanager-1", "127.0.0.1", "", ring.Tokens{1, 2, 3}, ring.ACTIVE, registeredAt)
				desc.AddIngester("alertmanager-2", "127.0.0.2", "", ring.Tokens{4, 5, 6}, ring.ACTIVE, registeredAt)

				instance := desc.Ingesters["alertmanager-2"]
				instance.Timestamp = time.Now().Add(-time.Hour).Unix()
				desc.Ingesters["alertmanager-2"] = instance
			},
			updateRing: func(desc *ring.Desc) {
				desc.RemoveIngester("alertmanager-2")
			},
			expected: false,
		},
	}

	for _, tt := range tc {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			amConfig := mockAlertmanagerConfig(t)
			amConfig.ShardingEnabled = true
			amConfig.ShardingRing.RingCheckPeriod = 100 * time.Millisecond
			amConfig.PollInterval = time.Hour // Don't trigger the periodic check.

			ringStore := consul.NewInMemoryClient(ring.GetCodec())
			alertStore := prepareInMemoryAlertStore()

			reg := prometheus.NewPedanticRegistry()
			am, err := createMultitenantAlertmanager(amConfig, nil, nil, alertStore, ringStore, log.NewNopLogger(), reg)
			require.NoError(t, err)

			require.NoError(t, ringStore.CAS(ctx, RingKey, func(in interface{}) (interface{}, bool, error) {
				ringDesc := ring.GetOrCreateRingDesc(in)
				tt.setupRing(ringDesc)
				return ringDesc, true, nil
			}))

			require.NoError(t, services.StartAndAwaitRunning(ctx, am))
			defer services.StopAndAwaitTerminated(ctx, am) //nolint:errcheck

			// Make sure the initial sync happened.
			regs := util.NewUserRegistries()
			regs.AddUserRegistry("test", reg)
			metrics := regs.BuildMetricFamiliesPerUser()
			assert.Equal(t, float64(1), metrics.GetSumOfCounters("cortex_alertmanager_sync_configs_total"))

			// Change the ring topology.
			require.NoError(t, ringStore.CAS(ctx, RingKey, func(in interface{}) (interface{}, bool, error) {
				ringDesc := ring.GetOrCreateRingDesc(in)
				tt.updateRing(ringDesc)
				return ringDesc, true, nil
			}))

			// Assert if we expected a sync or not.
			if tt.expected {
				test.Poll(t, time.Second, float64(2), func() interface{} {
					metrics := regs.BuildMetricFamiliesPerUser()
					return metrics.GetSumOfCounters("cortex_alertmanager_sync_configs_total")
				})
			} else {
				time.Sleep(250 * time.Millisecond)

				metrics := regs.BuildMetricFamiliesPerUser()
				assert.Equal(t, float64(1), metrics.GetSumOfCounters("cortex_alertmanager_sync_configs_total"))
			}
		})
	}
}

func TestMultitenantAlertmanager_RingLifecyclerShouldAutoForgetUnhealthyInstances(t *testing.T) {
	const unhealthyInstanceID = "alertmanager-bad-1"
	const heartbeatTimeout = time.Minute
	ctx := context.Background()
	amConfig := mockAlertmanagerConfig(t)
	amConfig.ShardingEnabled = true
	amConfig.ShardingRing.HeartbeatPeriod = 100 * time.Millisecond
	amConfig.ShardingRing.HeartbeatTimeout = heartbeatTimeout

	ringStore := consul.NewInMemoryClient(ring.GetCodec())
	alertStore := prepareInMemoryAlertStore()

	am, err := createMultitenantAlertmanager(amConfig, nil, nil, alertStore, ringStore, log.NewNopLogger(), nil)
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(ctx, am))
	defer services.StopAndAwaitTerminated(ctx, am) //nolint:errcheck

	require.NoError(t, ringStore.CAS(ctx, RingKey, func(in interface{}) (interface{}, bool, error) {
		ringDesc := ring.GetOrCreateRingDesc(in)
		instance := ringDesc.AddIngester(unhealthyInstanceID, "127.0.0.1", "", ring.GenerateTokens(RingNumTokens, nil), ring.ACTIVE, time.Now())
		instance.Timestamp = time.Now().Add(-(ringAutoForgetUnhealthyPeriods + 1) * heartbeatTimeout).Unix()
		ringDesc.Ingesters[unhealthyInstanceID] = instance

		return ringDesc, true, nil
	}))

	test.Poll(t, time.Second, false, func() interface{} {
		d, err := ringStore.Get(ctx, RingKey)
		if err != nil {
			return err
		}

		_, ok := ring.GetOrCreateRingDesc(d).Ingesters[unhealthyInstanceID]
		return ok
	})
}

func TestMultitenantAlertmanager_InitialSyncFailureWithSharding(t *testing.T) {
	ctx := context.Background()
	amConfig := mockAlertmanagerConfig(t)
	amConfig.ShardingEnabled = true
	ringStore := consul.NewInMemoryClient(ring.GetCodec())

	// Mock the store to fail listing configs.
	bkt := &bucket.ClientMock{}
	bkt.MockIter("alerts/", nil, errors.New("failed to list alerts"))
	store := bucketclient.NewBucketAlertStore(bkt, nil, log.NewNopLogger())

	am, err := createMultitenantAlertmanager(amConfig, nil, nil, store, ringStore, log.NewNopLogger(), nil)
	require.NoError(t, err)
	defer services.StopAndAwaitTerminated(ctx, am) //nolint:errcheck

	require.NoError(t, am.StartAsync(ctx))
	err = am.AwaitRunning(ctx)
	require.Error(t, err)
	require.Equal(t, services.Failed, am.State())
	require.False(t, am.ringLifecycler.IsRegistered())
	require.NotNil(t, am.ring)
}

func TestAlertmanager_ReplicasPosition(t *testing.T) {
	ctx := context.Background()
	ringStore := consul.NewInMemoryClient(ring.GetCodec())
	mockStore := prepareInMemoryAlertStore()
	require.NoError(t, mockStore.SetAlertConfig(ctx, alertspb.AlertConfigDesc{
		User:      "user-1",
		RawConfig: simpleConfigOne,
		Templates: []*alertspb.TemplateDesc{},
	}))

	var instances []*MultitenantAlertmanager
	var instanceIDs []string
	registries := util.NewUserRegistries()

	// First, create the alertmanager instances, we'll use a replication factor of 3 and create 3 instances so that we can get the tenant on each replica.
	for i := 1; i <= 3; i++ {
		//instanceIDs = append(instanceIDs, fmt.Sprintf("alertmanager-%d", i))
		instanceID := fmt.Sprintf("alertmanager-%d", i)

		amConfig := mockAlertmanagerConfig(t)
		amConfig.ShardingRing.ReplicationFactor = 3
		amConfig.ShardingRing.InstanceID = instanceID
		amConfig.ShardingRing.InstanceAddr = fmt.Sprintf("127.0.0.%d", i)

		// Do not check the ring topology changes or poll in an interval in this test (we explicitly sync alertmanagers).
		amConfig.PollInterval = time.Hour
		amConfig.ShardingRing.RingCheckPeriod = time.Hour
		amConfig.ShardingEnabled = true

		reg := prometheus.NewPedanticRegistry()
		am, err := createMultitenantAlertmanager(amConfig, nil, nil, mockStore, ringStore, log.NewNopLogger(), reg)
		require.NoError(t, err)
		defer services.StopAndAwaitTerminated(ctx, am) //nolint:errcheck

		require.NoError(t, services.StartAndAwaitRunning(ctx, am))

		instances = append(instances, am)
		instanceIDs = append(instanceIDs, instanceID)
		registries.AddUserRegistry(instanceID, reg)
	}

	// We need make sure the ring is settled. The alertmanager is ready to be tested once all instances are ACTIVE and the ring settles.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	for _, am := range instances {
		for _, id := range instanceIDs {
			require.NoError(t, ring.WaitInstanceState(ctx, am.ring, id, ring.ACTIVE))
		}
	}

	// Now that the ring has settled, sync configs with the instances.
	for _, am := range instances {
		err := am.loadAndSyncConfigs(ctx, reasonRingChange)
		require.NoError(t, err)
	}

	// Now that the ring has settled, we expect each AM instance to have a different position.
	// Let's walk through them and collect the positions.
	var positions []int
	for _, instance := range instances {
		instance.alertmanagersMtx.Lock()
		am, ok := instance.alertmanagers["user-1"]
		require.True(t, ok)
		positions = append(positions, am.state.Position())
		instance.alertmanagersMtx.Unlock()
	}

	require.ElementsMatch(t, []int{0, 1, 2}, positions)
}

func TestAlertmanager_StateReplicationWithSharding(t *testing.T) {
	t.Skip("Flaky test under investigation")

	tc := []struct {
		name              string
		replicationFactor int
		instances         int
		withSharding      bool
	}{
		{
			name:              "sharding disabled (hence no replication factor),  1 instance",
			withSharding:      false,
			instances:         1,
			replicationFactor: 0,
		},
		{
			name:              "sharding enabled, RF = 2, 2 instances",
			withSharding:      true,
			instances:         2,
			replicationFactor: 2,
		},
		{
			name:              "sharding enabled, RF = 3, 10 instance",
			withSharding:      true,
			instances:         10,
			replicationFactor: 3,
		},
	}

	for _, tt := range tc {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ringStore := consul.NewInMemoryClient(ring.GetCodec())
			mockStore := prepareInMemoryAlertStore()
			clientPool := newPassthroughAlertmanagerClientPool()
			externalURL := flagext.URLValue{}
			err := externalURL.Set("http://localhost:8080/alertmanager")
			require.NoError(t, err)

			var instances []*MultitenantAlertmanager
			var instanceIDs []string
			registries := util.NewUserRegistries()

			// First, add the number of configs to the store.
			for i := 1; i <= 12; i++ {
				u := fmt.Sprintf("u-%d", i)
				require.NoError(t, mockStore.SetAlertConfig(ctx, alertspb.AlertConfigDesc{
					User:      u,
					RawConfig: simpleConfigOne,
					Templates: []*alertspb.TemplateDesc{},
				}))
			}

			// Then, create the alertmanager instances, start them and add their registries to the slice.
			for i := 1; i <= tt.instances; i++ {
				instanceIDs = append(instanceIDs, fmt.Sprintf("alertmanager-%d", i))
				instanceID := fmt.Sprintf("alertmanager-%d", i)

				amConfig := mockAlertmanagerConfig(t)
				amConfig.ExternalURL = externalURL
				amConfig.ShardingRing.ReplicationFactor = tt.replicationFactor
				amConfig.ShardingRing.InstanceID = instanceID
				amConfig.ShardingRing.InstanceAddr = fmt.Sprintf("127.0.0.%d", i)

				// Do not check the ring topology changes or poll in an interval in this test (we explicitly sync alertmanagers).
				amConfig.PollInterval = time.Hour
				amConfig.ShardingRing.RingCheckPeriod = time.Hour

				if tt.withSharding {
					amConfig.ShardingEnabled = true
				}

				reg := prometheus.NewPedanticRegistry()
				am, err := createMultitenantAlertmanager(amConfig, nil, nil, mockStore, ringStore, log.NewNopLogger(), reg)
				require.NoError(t, err)
				defer services.StopAndAwaitTerminated(ctx, am) //nolint:errcheck

				if tt.withSharding {
					clientPool.servers[amConfig.ShardingRing.InstanceAddr+":0"] = am
					am.alertmanagerClientsPool = clientPool
				}

				require.NoError(t, services.StartAndAwaitRunning(ctx, am))

				instances = append(instances, am)
				instanceIDs = append(instanceIDs, instanceID)
				registries.AddUserRegistry(instanceID, reg)
			}

			// If we're testing with sharding, we need make sure the ring is settled.
			if tt.withSharding {
				ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()

				// The alertmanager is ready to be tested once all instances are ACTIVE and the ring settles.
				for _, am := range instances {
					for _, id := range instanceIDs {
						require.NoError(t, ring.WaitInstanceState(ctx, am.ring, id, ring.ACTIVE))
					}
				}
			}

			// Now that the ring has settled, sync configs with the instances.
			var numConfigs, numInstances int
			for _, am := range instances {
				err := am.loadAndSyncConfigs(ctx, reasonRingChange)
				require.NoError(t, err)
				numConfigs += len(am.cfgs)
				numInstances += len(am.alertmanagers)
			}

			// With sharding enabled, we propagate messages over gRPC instead of using a gossip over TCP.
			// 1. First, get a random multitenant instance
			multitenantAM := instances[rand.Intn(len(instances))]

			// 2. Then, get a random user that exists in that particular alertmanager instance.
			multitenantAM.alertmanagersMtx.Lock()
			k := rand.Intn(len(multitenantAM.alertmanagers))
			var userID string
			for u := range multitenantAM.alertmanagers {
				if k == 0 {
					userID = u
					break
				}
				k--
			}
			multitenantAM.alertmanagersMtx.Unlock()

			// 3. Now that we have our alertmanager user, let's create a silence and make sure it is replicated.
			silence := types.Silence{
				Matchers: labels.Matchers{
					{Name: "instance", Value: "prometheus-one"},
				},
				Comment:  "Created for a test case.",
				StartsAt: time.Now(),
				EndsAt:   time.Now().Add(time.Hour),
			}
			data, err := json.Marshal(silence)
			require.NoError(t, err)

			// 4. Create the silence in one of the alertmanagers
			req := httptest.NewRequest(http.MethodPost, externalURL.String()+"/api/v2/silences", bytes.NewReader(data))
			req.Header.Set("content-type", "application/json")
			reqCtx := user.InjectOrgID(req.Context(), userID)
			{
				w := httptest.NewRecorder()
				multitenantAM.ServeHTTP(w, req.WithContext(reqCtx))

				resp := w.Result()
				body, _ := ioutil.ReadAll(resp.Body)
				assert.Equal(t, http.StatusOK, w.Code)
				require.Regexp(t, regexp.MustCompile(`{"silenceID":".+"}`), string(body))
			}

			metrics := registries.BuildMetricFamiliesPerUser()

			// If sharding is not enabled, we never propagate any messages amongst replicas in this way, and we can stop here.
			if !tt.withSharding {
				assert.Equal(t, float64(1), metrics.GetSumOfGauges("cortex_alertmanager_silences"))
				assert.Equal(t, float64(0), metrics.GetSumOfCounters("cortex_alertmanager_state_replication_total"))
				assert.Equal(t, float64(0), metrics.GetSumOfCounters("cortex_alertmanager_state_replication_failed_total"))
				return
			}

			// 5. Then, make sure it is propagated successfully.
			assert.Equal(t, float64(tt.replicationFactor), metrics.GetSumOfGauges("cortex_alertmanager_silences"))
			assert.Equal(t, float64(tt.replicationFactor), metrics.GetSumOfCounters("cortex_alertmanager_state_replication_total"))
			assert.Equal(t, float64(0), metrics.GetSumOfCounters("cortex_alertmanager_state_replication_failed_total"))

			// 5b. Check the number of partial states merged are as we expect.
			// Partial states are currently replicated twice:
			//   For RF=1 1 -> 0      = Total 0 merges
			//   For RF=2 1 -> 1 -> 1 = Total 2 merges
			//   For RF=3 1 -> 2 -> 4 = Total 6 merges
			nFanOut := tt.replicationFactor - 1
			nMerges := nFanOut + (nFanOut * nFanOut)
			assert.Equal(t, float64(nMerges), metrics.GetSumOfCounters("cortex_alertmanager_partial_state_merges_total"))
			assert.Equal(t, float64(0), metrics.GetSumOfCounters("cortex_alertmanager_partial_state_merges_failed_total"))
		})
	}
}

func TestAlertmanager_StateReplicationWithSharding_InitialSyncFromPeers(t *testing.T) {
	tc := []struct {
		name              string
		replicationFactor int
	}{
		{
			name:              "RF = 2",
			replicationFactor: 2,
		},
		{
			name:              "RF = 3",
			replicationFactor: 3,
		},
	}

	for _, tt := range tc {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ringStore := consul.NewInMemoryClient(ring.GetCodec())
			mockStore := prepareInMemoryAlertStore()
			clientPool := newPassthroughAlertmanagerClientPool()
			externalURL := flagext.URLValue{}
			err := externalURL.Set("http://localhost:8080/alertmanager")
			require.NoError(t, err)

			var instances []*MultitenantAlertmanager
			var instanceIDs []string
			registries := util.NewUserRegistries()

			// Create only two users - no need for more for these test cases.
			for i := 1; i <= 2; i++ {
				u := fmt.Sprintf("u-%d", i)
				require.NoError(t, mockStore.SetAlertConfig(ctx, alertspb.AlertConfigDesc{
					User:      u,
					RawConfig: simpleConfigOne,
					Templates: []*alertspb.TemplateDesc{},
				}))
			}

			createInstance := func(i int) *MultitenantAlertmanager {
				instanceIDs = append(instanceIDs, fmt.Sprintf("alertmanager-%d", i))
				instanceID := fmt.Sprintf("alertmanager-%d", i)

				amConfig := mockAlertmanagerConfig(t)
				amConfig.ExternalURL = externalURL
				amConfig.ShardingRing.ReplicationFactor = tt.replicationFactor
				amConfig.ShardingRing.InstanceID = instanceID
				amConfig.ShardingRing.InstanceAddr = fmt.Sprintf("127.0.0.%d", i)

				// Do not check the ring topology changes or poll in an interval in this test (we explicitly sync alertmanagers).
				amConfig.PollInterval = time.Hour
				amConfig.ShardingRing.RingCheckPeriod = time.Hour

				amConfig.ShardingEnabled = true

				reg := prometheus.NewPedanticRegistry()
				am, err := createMultitenantAlertmanager(amConfig, nil, nil, mockStore, ringStore, log.NewNopLogger(), reg)
				require.NoError(t, err)

				clientPool.servers[amConfig.ShardingRing.InstanceAddr+":0"] = am
				am.alertmanagerClientsPool = clientPool

				require.NoError(t, services.StartAndAwaitRunning(ctx, am))
				t.Cleanup(func() {
					require.NoError(t, services.StopAndAwaitTerminated(ctx, am))
				})

				instances = append(instances, am)
				instanceIDs = append(instanceIDs, instanceID)
				registries.AddUserRegistry(instanceID, reg)

				// Make sure the ring is settled.
				{
					ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
					defer cancel()

					// The alertmanager is ready to be tested once all instances are ACTIVE and the ring settles.
					for _, am := range instances {
						for _, id := range instanceIDs {
							require.NoError(t, ring.WaitInstanceState(ctx, am.ring, id, ring.ACTIVE))
						}
					}
				}

				// Now that the ring has settled, sync configs with the instances.
				require.NoError(t, am.loadAndSyncConfigs(ctx, reasonRingChange))

				return am
			}

			writeSilence := func(i *MultitenantAlertmanager, userID string) {
				silence := types.Silence{
					Matchers: labels.Matchers{
						{Name: "instance", Value: "prometheus-one"},
					},
					Comment:  "Created for a test case.",
					StartsAt: time.Now(),
					EndsAt:   time.Now().Add(time.Hour),
				}
				data, err := json.Marshal(silence)
				require.NoError(t, err)

				req := httptest.NewRequest(http.MethodPost, externalURL.String()+"/api/v2/silences", bytes.NewReader(data))
				req.Header.Set("content-type", "application/json")
				reqCtx := user.InjectOrgID(req.Context(), userID)
				{
					w := httptest.NewRecorder()
					i.ServeHTTP(w, req.WithContext(reqCtx))

					resp := w.Result()
					body, _ := ioutil.ReadAll(resp.Body)
					assert.Equal(t, http.StatusOK, w.Code)
					require.Regexp(t, regexp.MustCompile(`{"silenceID":".+"}`), string(body))
				}
			}

			checkSilence := func(i *MultitenantAlertmanager, userID string) {
				req := httptest.NewRequest(http.MethodGet, externalURL.String()+"/api/v2/silences", nil)
				req.Header.Set("content-type", "application/json")
				reqCtx := user.InjectOrgID(req.Context(), userID)
				{
					w := httptest.NewRecorder()
					i.ServeHTTP(w, req.WithContext(reqCtx))

					resp := w.Result()
					body, _ := ioutil.ReadAll(resp.Body)
					assert.Equal(t, http.StatusOK, w.Code)
					require.Regexp(t, regexp.MustCompile(`"comment":"Created for a test case."`), string(body))
				}
			}

			// 1. Create the first instance and load the user configurations.
			i1 := createInstance(1)

			// 2. Create a silence in the first alertmanager instance and check we can read it.
			writeSilence(i1, "u-1")
			// 2.a. Check the silence was created (paranoia).
			checkSilence(i1, "u-1")
			// 2.b. Check the relevant metrics were updated.
			{
				metrics := registries.BuildMetricFamiliesPerUser()
				assert.Equal(t, float64(1), metrics.GetSumOfGauges("cortex_alertmanager_silences"))
				assert.Equal(t, float64(1), metrics.GetSumOfCounters("cortex_alertmanager_state_replication_total"))
				assert.Equal(t, float64(0), metrics.GetSumOfCounters("cortex_alertmanager_state_replication_failed_total"))
			}

			// 3. Create a second instance. This should attempt to fetch the silence from the first.
			i2 := createInstance(2)

			// 3.a. Check the silence was fetched from the first instance successfully.
			checkSilence(i2, "u-1")

			// 3.b. Check the metrics: We should see the additional silences without any replication activity.
			{
				metrics := registries.BuildMetricFamiliesPerUser()
				assert.Equal(t, float64(2), metrics.GetSumOfGauges("cortex_alertmanager_silences"))
				assert.Equal(t, float64(1), metrics.GetSumOfCounters("cortex_alertmanager_state_replication_total"))
				assert.Equal(t, float64(0), metrics.GetSumOfCounters("cortex_alertmanager_state_replication_failed_total"))
			}

			if tt.replicationFactor >= 3 {
				// 4. When testing RF = 3, create a third instance, to test obtaining state from multiple places.
				i3 := createInstance(3)

				// 4.a. Check the silence was fetched one or both of the instances successfully.
				checkSilence(i3, "u-1")

				// 4.b. Check the metrics one more time. We should have three replicas of the silence.
				{
					metrics := registries.BuildMetricFamiliesPerUser()
					assert.Equal(t, float64(3), metrics.GetSumOfGauges("cortex_alertmanager_silences"))
					assert.Equal(t, float64(1), metrics.GetSumOfCounters("cortex_alertmanager_state_replication_total"))
					assert.Equal(t, float64(0), metrics.GetSumOfCounters("cortex_alertmanager_state_replication_failed_total"))
				}
			}
		})
	}
}

// prepareInMemoryAlertStore builds and returns an in-memory alert store.
func prepareInMemoryAlertStore() alertstore.AlertStore {
	return bucketclient.NewBucketAlertStore(objstore.NewInMemBucket(), nil, log.NewNopLogger())
}

func TestStoreTemplateFile(t *testing.T) {
	tempDir, err := ioutil.TempDir(os.TempDir(), "alertmanager")
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, os.RemoveAll(tempDir))
	})

	testTemplateDir := filepath.Join(tempDir, templatesDir)

	changed, err := storeTemplateFile(testTemplateDir, "some-template", "content")
	require.NoError(t, err)
	require.True(t, changed)

	changed, err = storeTemplateFile(testTemplateDir, "some-template", "new content")
	require.NoError(t, err)
	require.True(t, changed)

	changed, err = storeTemplateFile(testTemplateDir, "some-template", "new content") // reusing previous content
	require.NoError(t, err)
	require.False(t, changed)

	_, err = storeTemplateFile(testTemplateDir, ".", "content")
	require.Error(t, err)

	_, err = storeTemplateFile(testTemplateDir, "..", "content")
	require.Error(t, err)

	_, err = storeTemplateFile(testTemplateDir, "./test", "content")
	require.Error(t, err)

	_, err = storeTemplateFile(testTemplateDir, "../test", "content")
	require.Error(t, err)
}

type passthroughAlertmanagerClient struct {
	server alertmanagerpb.AlertmanagerServer
}

func (am *passthroughAlertmanagerClient) UpdateState(ctx context.Context, in *clusterpb.Part, opts ...grpc.CallOption) (*alertmanagerpb.UpdateStateResponse, error) {
	return am.server.UpdateState(ctx, in)
}

func (am *passthroughAlertmanagerClient) ReadState(ctx context.Context, in *alertmanagerpb.ReadStateRequest, opts ...grpc.CallOption) (*alertmanagerpb.ReadStateResponse, error) {
	return am.server.ReadState(ctx, in)
}

func (am *passthroughAlertmanagerClient) HandleRequest(context.Context, *httpgrpc.HTTPRequest, ...grpc.CallOption) (*httpgrpc.HTTPResponse, error) {
	return nil, fmt.Errorf("unexpected call to HandleRequest")
}

func (am *passthroughAlertmanagerClient) RemoteAddress() string {
	return ""
}

// passthroughAlertmanagerClientPool allows testing the logic of gRPC calls between alertmanager instances
// by invoking client calls directly to a peer instance in the unit test, without the server running.
type passthroughAlertmanagerClientPool struct {
	servers map[string]alertmanagerpb.AlertmanagerServer
}

func newPassthroughAlertmanagerClientPool() *passthroughAlertmanagerClientPool {
	return &passthroughAlertmanagerClientPool{
		servers: make(map[string]alertmanagerpb.AlertmanagerServer),
	}
}

func (f *passthroughAlertmanagerClientPool) GetClientFor(addr string) (Client, error) {
	s, ok := f.servers[addr]
	if !ok {
		return nil, fmt.Errorf("client not found for address: %v", addr)
	}
	return Client(&passthroughAlertmanagerClient{s}), nil
}