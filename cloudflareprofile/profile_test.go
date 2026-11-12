package cloudflareprofile

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateFailClosedDoesNotEchoSecrets(t *testing.T) {
	const secret = "supersecret-do-not-leak"
	getenv := func(overrides map[string]string) func(string) string {
		return func(key string) string {
			if value, ok := overrides[key]; ok {
				return value
			}
			return ""
		}
	}
	const databaseHost = "ep-aged-sunset-b33os2qw.c-4.ap-southeast-1.aws.neon.tech"
	base := map[string]string{
		"CLOUDFLARE_PROFILE":        "1",
		"SQL_DSN":                   "postgres://user:" + secret + "@" + databaseHost + "/needtoken",
		"LOG_SQL_DSN":               "postgres://user:" + secret + "@" + databaseHost + "/needtoken",
		"SESSION_SECRET":            "session-" + secret,
		"CRYPTO_SECRET":             "crypto-" + secret,
		"NEEDTOKEN_LOG_SQL":         "main",
		"REDIS_CONN_STRING":         "rediss://default:" + secret + "@us1-example.upstash.io:6379",
		"DATABASE_HOST":             databaseHost,
		"NEEDTOKEN_WRITER_PERMIT":   "staging-writer",
		"NEEDTOKEN_QUEUE_CONSUMERS": "off",
		"NEEDTOKEN_RUNTIME":         RuntimeStaging,
	}

	t.Run("profile off does nothing", func(t *testing.T) {
		off := map[string]string{}
		require.NoError(t, Validate(getenv(off)))
		require.False(t, ForbidsSQLite(getenv(off), ""))
	})

	t.Run("absent SQL_DSN", func(t *testing.T) {
		values := cloneMap(base)
		values["SQL_DSN"] = "  "
		err := Validate(getenv(values))
		require.EqualError(t, err, "cloudflare profile refuses to start: SQL_DSN binding is absent")
		require.NotContains(t, err.Error(), secret)
	})

	t.Run("sqlite fallback", func(t *testing.T) {
		for _, dsn := range []string{"", "local", "local-" + secret} {
			require.True(t, ForbidsSQLite(getenv(base), dsn))
		}
		require.False(t, ForbidsSQLite(getenv(base), base["SQL_DSN"]))
		values := cloneMap(base)
		values["SQL_DSN"] = "local-" + secret
		err := Validate(getenv(values))
		require.EqualError(t, err, "cloudflare profile refuses SQLite")
		require.NotContains(t, err.Error(), secret)
	})

	t.Run("absent session", func(t *testing.T) {
		values := cloneMap(base)
		values["SESSION_SECRET"] = ""
		err := Validate(getenv(values))
		require.EqualError(t, err, "cloudflare profile refuses to start: SESSION_SECRET binding is absent")
		require.NotContains(t, err.Error(), secret)
	})

	t.Run("placeholder session", func(t *testing.T) {
		values := cloneMap(base)
		values["SESSION_SECRET"] = "random_string"
		require.EqualError(t, Validate(getenv(values)), "cloudflare profile refuses the placeholder SESSION_SECRET")
	})

	t.Run("absent redis is not a memory store", func(t *testing.T) {
		values := cloneMap(base)
		values["REDIS_CONN_STRING"] = ""
		err := Validate(getenv(values))
		require.EqualError(t, err, "cloudflare profile refuses to start without REDIS_CONN_STRING")
		require.NotContains(t, err.Error(), secret)
	})

	t.Run("sqlite path pprof and default token", func(t *testing.T) {
		cases := []struct {
			key    string
			value  string
			expect string
		}{
			{"SQLITE_PATH", "/data/" + secret + ".db", "cloudflare profile refuses SQLITE_PATH"},
			{"ENABLE_PPROF", "true", "cloudflare profile refuses ENABLE_PPROF disk profiles"},
			{"GENERATE_DEFAULT_TOKEN", "true", "cloudflare profile refuses generating a default token"},
		}
		for _, tc := range cases {
			values := cloneMap(base)
			values[tc.key] = tc.value
			err := Validate(getenv(values))
			require.EqualError(t, err, tc.expect)
			require.NotContains(t, err.Error(), secret)
		}
	})

	t.Run("writer mode is not production", func(t *testing.T) {
		values := cloneMap(base)
		values["NEEDTOKEN_RUNTIME"] = "production"
		err := Validate(getenv(values))
		require.EqualError(t, err, "cloudflare profile refuses to start a writer")
		require.NotContains(t, err.Error(), secret)
	})

	t.Run("a test permit does not open SQL", func(t *testing.T) {
		for _, permit := range []string{"", "APPROVED", "test-fixture", "isolated.example"} {
			values := cloneMap(base)
			values["NEEDTOKEN_WRITER_PERMIT"] = permit
			err := Validate(getenv(values))
			require.Error(t, err)
			require.NotContains(t, err.Error(), secret)
		}
	})

	t.Run("every host outside the staging database is refused", func(t *testing.T) {
		for _, host := range []string{"ep-prod.neon.tech", "ep-prod.neon.tech.isolated", "db.example.test", "ep-other-pooler.c-4.ap-southeast-1.aws.neon.tech"} {
			values := cloneMap(base)
			values["SQL_DSN"] = "postgres://user:" + secret + "@" + host + "/needtoken"
			values["LOG_SQL_DSN"] = values["SQL_DSN"]
			values["DATABASE_HOST"] = host
			err := Validate(getenv(values))
			require.EqualError(t, err, "cloudflare profile refuses a SQL host outside the isolated allowlist")
			require.NotContains(t, err.Error(), secret)
			require.NotContains(t, err.Error(), host)
		}
		mismatch := cloneMap(base)
		mismatch["DATABASE_HOST"] = "ep-aged-sunset-b33os2qw-pooler.c-4.ap-southeast-1.aws.neon.tech"
		err := Validate(getenv(mismatch))
		require.EqualError(t, err, "cloudflare profile refuses a SQL host outside the isolated allowlist")
	})

	t.Run("the staging pooler is allowed and redis stays on upstash", func(t *testing.T) {
		values := cloneMap(base)
		pooler := "ep-aged-sunset-b33os2qw-pooler.c-4.ap-southeast-1.aws.neon.tech"
		values["SQL_DSN"] = "postgres://user:" + secret + "@" + pooler + "/needtoken"
		values["LOG_SQL_DSN"] = values["SQL_DSN"]
		values["DATABASE_HOST"] = pooler
		values["REDIS_HOST"] = "ignored.upstash.io"
		require.NoError(t, Validate(getenv(values)))
	})

	t.Run("queue consumers stay stopped", func(t *testing.T) {
		require.True(t, QueueConsumersStopped(getenv(base)))
		require.False(t, QueueConsumersStopped(getenv(map[string]string{})))
		for _, value := range []string{"", "on", "staging:"} {
			values := cloneMap(base)
			values["NEEDTOKEN_QUEUE_CONSUMERS"] = value
			err := Validate(getenv(values))
			require.EqualError(t, err, "cloudflare profile refuses to consume production queues")
		}
	})

	t.Run("missing crypto is not a session alias", func(t *testing.T) {
		values := cloneMap(base)
		delete(values, "CRYPTO_SECRET")
		err := Validate(getenv(values))
		require.EqualError(t, err, "cloudflare profile refuses to start: CRYPTO_SECRET binding is absent")
		require.Equal(t, "absent", CryptoSecretSource(getenv(values)))
		require.NotContains(t, err.Error(), secret)
	})

	t.Run("log database keeps an explicit meaning", func(t *testing.T) {
		missing := cloneMap(base)
		delete(missing, "NEEDTOKEN_LOG_SQL")
		err := Validate(getenv(missing))
		require.EqualError(t, err, "cloudflare profile refuses an implicit log database")

		conflict := cloneMap(base)
		conflict["LOG_SQL_DSN"] = "postgres://user:" + secret + "@logs.staging.internal/logs"
		err = Validate(getenv(conflict))
		require.EqualError(t, err, "cloudflare profile refuses LOG_SQL_DSN that is not the isolated main database")
		require.NotContains(t, err.Error(), secret)

		emptyMain := cloneMap(base)
		emptyMain["LOG_SQL_DSN"] = ""
		err = Validate(getenv(emptyMain))
		require.EqualError(t, err, "cloudflare profile refuses LOG_SQL_DSN that is not the isolated main database")

		separate := cloneMap(base)
		separate["NEEDTOKEN_LOG_SQL"] = "separate"
		separate["LOG_SQL_DSN"] = ""
		err = Validate(getenv(separate))
		require.EqualError(t, err, "cloudflare profile refuses a separate log database without LOG_SQL_DSN")

		separateHost := cloneMap(base)
		separateHost["NEEDTOKEN_LOG_SQL"] = "separate"
		separateHost["LOG_SQL_DSN"] = "postgres://user:" + secret + "@logs.example.test/logs"
		err = Validate(getenv(separateHost))
		require.EqualError(t, err, "cloudflare profile refuses a SQL host outside the isolated allowlist")
		require.NotContains(t, err.Error(), secret)
		pooler := "ep-aged-sunset-b33os2qw-pooler.c-4.ap-southeast-1.aws.neon.tech"
		separateHost["LOG_SQL_DSN"] = "postgres://user:" + secret + "@" + pooler + "/logs"
		require.NoError(t, Validate(getenv(separateHost)))
	})

	t.Run("bindings present", func(t *testing.T) {
		require.NoError(t, Validate(getenv(base)))
		require.Equal(t, "binding", CryptoSecretSource(getenv(base)))
	})
}

func TestDiskProfileAndBootstrap(t *testing.T) {
	require.NoError(t, RejectDiskProfile(true, true))
	require.NoError(t, RejectDiskProfile(false, false))
	require.EqualError(t, RejectDiskProfile(false, true), "cloudflare profile must be injected by the process environment")
	require.NoError(t, RefuseBootstrap(true))
	require.EqualError(t, RefuseBootstrap(false), "cloudflare profile refuses bootstrap")
}

func TestDataDirFailClosed(t *testing.T) {
	var required int
	for _, rule := range DataInventory() {
		if rule.Class == DataRequired {
			required++
		}
		require.NotEmpty(t, rule.Evidence)
	}
	require.Zero(t, required, "a file that is not proven disposable must be DataRequired and then fail when missing")
	require.NoError(t, EvaluateDataNames(nil))
	require.NoError(t, EvaluateDataNames([]string{".env", "logs", "pprof", "new-api-body-cache"}))

	err := EvaluateDataNames([]string{"one-api.db"})
	require.EqualError(t, err, "cloudflare profile refuses a SQLite data file: one-api.db")
	err = EvaluateDataNames([]string{"one-api.db-wal"})
	require.EqualError(t, err, "cloudflare profile refuses a SQLite data file: one-api.db-wal")
	err = EvaluateDataNames([]string{"custom-state.bin"})
	require.EqualError(t, err, "cloudflare profile refuses an unclassified data file: custom-state.bin")

	missing := t.TempDir() + "/does-not-exist"
	require.NoError(t, ScanPath(missing))

	dir := t.TempDir()
	require.NoError(t, ScanPath(dir))
	require.NoError(t, ScanDataDir(func(string) string { return "" }))

	getenv := func(key string) string {
		if key == "CLOUDFLARE_PROFILE" {
			return "1"
		}
		if key == "CLOUDFLARE_DATA_DIR" {
			return dir
		}
		return ""
	}
	require.NoError(t, ScanDataDir(getenv))
}

func TestStdoutLogsAndRedisRefusal(t *testing.T) {
	t.Run("profile clears the log directory", func(t *testing.T) {
		t.Setenv("CLOUDFLARE_PROFILE", "1")
		previous := *common.LogDir
		t.Cleanup(func() { *common.LogDir = previous })
		*common.LogDir = "/data/logs"
		common.UseStdoutLogsForCloudflareProfile()
		require.Empty(t, *common.LogDir)
	})

	t.Run("default runtime keeps the log directory", func(t *testing.T) {
		t.Setenv("CLOUDFLARE_PROFILE", "")
		previous := *common.LogDir
		t.Cleanup(func() { *common.LogDir = previous })
		*common.LogDir = "/data/logs"
		common.UseStdoutLogsForCloudflareProfile()
		require.Equal(t, "/data/logs", *common.LogDir)
	})

	t.Run("profile does not disable redis into memory", func(t *testing.T) {
		previous := common.RedisEnabled
		t.Cleanup(func() { common.RedisEnabled = previous })
		common.RedisEnabled = true
		t.Setenv("CLOUDFLARE_PROFILE", "1")
		t.Setenv("REDIS_CONN_STRING", "")
		err := common.InitRedisClient()
		require.EqualError(t, err, "cloudflare profile refuses to start without REDIS_CONN_STRING")
		assert.True(t, common.RedisEnabled)
	})

	t.Run("default runtime still allows redis to be off", func(t *testing.T) {
		previous := common.RedisEnabled
		t.Cleanup(func() { common.RedisEnabled = previous })
		common.RedisEnabled = true
		t.Setenv("CLOUDFLARE_PROFILE", "")
		t.Setenv("REDIS_CONN_STRING", "")
		require.NoError(t, common.InitRedisClient())
		assert.False(t, common.RedisEnabled)
	})
}

func TestStartRefusesNilRedis(t *testing.T) {
	_, err := Start(context.Background(), nil)
	require.EqualError(t, err, "cloudflare profile refuses to start without Redis")
}

func TestStagingWriterLeaseKeysDoNotMatchProduction(t *testing.T) {
	require.Contains(t, writerLeaseKey, "staging")
	require.Contains(t, writerEpochKey, "staging")
	require.NotEqual(t, "needtoken-api:cloudflare:writer-lease", writerLeaseKey)
	require.NotEqual(t, "needtoken-api:cloudflare:writer-epoch", writerEpochKey)

	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	epoch, err := newRedisLease(client).Acquire(context.Background(), "holder-a", time.Minute)
	require.NoError(t, err)
	require.Greater(t, epoch, int64(0))
	require.True(t, server.Exists(writerLeaseKey))
	require.True(t, server.Exists(writerEpochKey))
	require.False(t, server.Exists("needtoken-api:cloudflare:writer-lease"))
	require.False(t, server.Exists("needtoken-api:cloudflare:writer-epoch"))
}

func TestLeaseTextRoundTrip(t *testing.T) {
	value := formatLease(7, "abc")
	epoch, holder, ok := parseLease(value)
	require.True(t, ok)
	assert.Equal(t, int64(7), epoch)
	assert.Equal(t, "abc", holder)
	_, _, ok = parseLease("no-separator")
	assert.False(t, ok)
	require.Contains(t, acquireScript, "NX")
	require.Contains(t, acquireScript, "INCR")
	require.Contains(t, renewScript, "PEXPIRE")
	require.Contains(t, releaseScript, "DEL")
}

func TestZeroUserTrafficDoesNotStopBackgroundOrReleaseFence(t *testing.T) {
	lease := newFakeLease()
	writer := newRuntime(lease, "writer-a", 30*time.Second)
	other := newRuntime(lease, "writer-b", 30*time.Second)
	ctx := context.Background()
	require.NoError(t, writer.Acquire(ctx))

	for range 5 {
		require.NoError(t, writer.Beat(ctx))
	}
	status := writer.Status()
	assert.Equal(t, int64(0), status.UserRequests)
	assert.Equal(t, int64(5), status.SupervisorBeats)
	assert.True(t, status.BackgroundRunning)
	assert.True(t, status.FenceHeld)
	assert.True(t, status.Ready)
	assert.False(t, status.Draining)
	assert.Equal(t, int64(1), status.WriterEpoch)

	err := other.Acquire(ctx)
	require.ErrorIs(t, err, ErrWriterHeld)
	assert.Equal(t, int64(5), writer.Status().SupervisorBeats)
}

func TestRestartAfterExpiryGetsNextEpoch(t *testing.T) {
	lease := newFakeLease()
	writer := newRuntime(lease, "writer-a", 30*time.Second)
	ctx := context.Background()
	require.NoError(t, writer.Acquire(ctx))
	require.NoError(t, writer.Beat(ctx))
	lost := 0
	writer.SetOnLost(func() { lost++ })

	lease.Advance(30 * time.Second)
	recovered := newRuntime(lease, "writer-b", 30*time.Second)
	require.NoError(t, recovered.Acquire(ctx))
	assert.Equal(t, int64(2), recovered.Status().WriterEpoch)

	err := writer.Beat(ctx)
	require.ErrorIs(t, err, ErrWriterLost)
	assert.Equal(t, 1, lost)
	require.ErrorIs(t, writer.Beat(ctx), ErrWriterLost)
	assert.Equal(t, 1, lost)
	assert.False(t, writer.Status().BackgroundRunning)
	assert.False(t, writer.Status().FenceHeld)
	assert.Equal(t, int64(1), writer.Status().SupervisorBeats)

	require.NoError(t, recovered.Beat(ctx))
	assert.Equal(t, int64(0), recovered.Status().UserRequests)
	assert.True(t, recovered.Status().BackgroundRunning)
	err = writer.Acquire(ctx)
	require.ErrorIs(t, err, ErrWriterHeld)
}

func TestDrainKeepsFenceUntilIdleThenNextEpochCanRecover(t *testing.T) {
	lease := newFakeLease()
	writer := newRuntime(lease, "writer-a", 30*time.Second)
	other := newRuntime(lease, "writer-b", 30*time.Second)
	ctx := context.Background()
	require.NoError(t, writer.Acquire(ctx))
	require.NoError(t, writer.Beat(ctx))

	writer.BeginDrain()
	require.NoError(t, writer.Beat(ctx))
	status := writer.Status()
	assert.Equal(t, int64(0), status.UserRequests)
	assert.True(t, status.Draining)
	assert.True(t, status.BackgroundRunning)
	assert.False(t, status.Ready)
	err := other.Acquire(ctx)
	require.ErrorIs(t, err, ErrWriterHeld)

	require.NoError(t, writer.WaitIdle(ctx))
	require.NoError(t, writer.StopAndRelease(ctx))
	require.NoError(t, other.Acquire(ctx))
	assert.Equal(t, int64(2), other.Status().WriterEpoch)
	require.ErrorIs(t, writer.Beat(ctx), ErrWriterLost)
}

func TestInFlightDrainBlocksTheNextWriter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	lease := newFakeLease()
	writer := newRuntime(lease, "writer-a", 30*time.Second)
	other := newRuntime(lease, "writer-b", 30*time.Second)
	ctx := context.Background()
	require.NoError(t, writer.Acquire(ctx))
	require.NoError(t, writer.Beat(ctx))

	release := make(chan struct{})
	started := make(chan struct{})
	engine := gin.New()
	engine.Use(writer.Gate())
	Mount(engine, writer)
	engine.GET("/v1/chat", func(c *gin.Context) {
		close(started)
		<-release
		c.Status(http.StatusNoContent)
	})

	done := make(chan int, 1)
	go func() {
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/chat", nil))
		done <- recorder.Code
	}()
	<-started
	require.Equal(t, 1, writer.InFlight())
	writer.BeginDrain()
	err := other.Acquire(ctx)
	require.ErrorIs(t, err, ErrWriterHeld)

	statusRecorder := httptest.NewRecorder()
	engine.ServeHTTP(statusRecorder, httptest.NewRequest(http.MethodGet, "/internal/cloudflare/runtime", nil))
	require.Equal(t, http.StatusOK, statusRecorder.Code)
	var status Status
	require.NoError(t, common.Unmarshal(statusRecorder.Body.Bytes(), &status))
	assert.Equal(t, int64(1), status.UserRequests)
	assert.True(t, status.BackgroundRunning)
	assert.True(t, status.Draining)

	blocked := httptest.NewRecorder()
	engine.ServeHTTP(blocked, httptest.NewRequest(http.MethodGet, "/v1/chat", nil))
	require.Equal(t, http.StatusServiceUnavailable, blocked.Code)

	close(release)
	require.Equal(t, http.StatusNoContent, <-done)
	require.NoError(t, writer.WaitIdle(ctx))
	require.Equal(t, 0, writer.InFlight())
	require.NoError(t, writer.StopAndRelease(ctx))
	require.NoError(t, other.Acquire(ctx))
	assert.Equal(t, int64(2), other.Status().WriterEpoch)
}

func TestUserTrafficStaysClosedUntilTheFirstBeat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	lease := newFakeLease()
	writer := newRuntime(lease, "writer-a", 30*time.Second)
	require.NoError(t, writer.Acquire(context.Background()))
	engine := gin.New()
	engine.Use(writer.Gate())
	Mount(engine, writer)
	engine.GET("/v1/chat", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	statusRecorder := httptest.NewRecorder()
	engine.ServeHTTP(statusRecorder, httptest.NewRequest(http.MethodGet, "/internal/cloudflare/runtime", nil))
	require.Equal(t, http.StatusOK, statusRecorder.Code)
	chat := httptest.NewRecorder()
	engine.ServeHTTP(chat, httptest.NewRequest(http.MethodGet, "/v1/chat", nil))
	require.Equal(t, http.StatusServiceUnavailable, chat.Code)
	assert.Equal(t, int64(0), writer.Status().UserRequests)

	require.NoError(t, writer.Beat(context.Background()))
	chat = httptest.NewRecorder()
	engine.ServeHTTP(chat, httptest.NewRequest(http.MethodGet, "/v1/chat", nil))
	require.Equal(t, http.StatusNoContent, chat.Code)
	assert.Equal(t, int64(1), writer.Status().UserRequests)
}

type fakeLease struct {
	mu        sync.Mutex
	now       time.Time
	epoch     int64
	holder    string
	heldEpoch int64
	expiry    time.Time
}

func newFakeLease() *fakeLease {
	return &fakeLease{now: time.Unix(0, 0).UTC()}
}

func (f *fakeLease) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func (f *fakeLease) expired() bool {
	return f.holder == "" || !f.now.Before(f.expiry)
}

func TestSQLPermitBackgroundStopAndGCPAreSeparate(t *testing.T) {
	ResetSQLPermitForTest()
	t.Cleanup(ResetSQLPermitForTest)
	common.ResetCloudflareBackgroundForTest()
	t.Cleanup(common.ResetCloudflareBackgroundForTest)
	t.Setenv("CLOUDFLARE_PROFILE", "1")

	require.EqualError(t, PermitSQL(), "cloudflare profile refuses SQL before isolation and the writer permit")
	require.EqualError(t, RefuseDefaultAdmin(), "cloudflare profile refuses the default root account")
	GrantSQLPermit()
	require.NoError(t, PermitSQL())

	ran := 0
	require.True(t, common.RunCloudflareBackground(func() { ran++ }))
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		common.RunCloudflareBackground(func() {
			close(started)
			<-release
		})
	}()
	<-started
	stopped := make(chan struct{})
	go func() {
		common.StopCloudflareBackgroundWrites()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("stop returned while a background write was still running")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	<-stopped
	<-finished
	require.False(t, common.RunCloudflareBackground(func() { ran++ }))
	assert.Equal(t, 1, ran)

	writer := newRuntime(newFakeLease(), "writer-a", time.Second)
	require.NoError(t, writer.Acquire(context.Background()))
	require.NoError(t, writer.Beat(context.Background()))
	assert.False(t, writer.Status().GCPFenced)
}

func (f *fakeLease) Acquire(ctx context.Context, holder string, ttl time.Duration) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.expired() && f.holder != holder {
		return 0, ErrWriterHeld
	}
	if f.expired() {
		f.epoch++
		f.heldEpoch = f.epoch
		f.holder = holder
	}
	f.expiry = f.now.Add(ttl)
	return f.heldEpoch, nil
}

func (f *fakeLease) Renew(ctx context.Context, holder string, epoch int64, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.holder != holder || f.heldEpoch != epoch || f.expired() {
		return ErrWriterLost
	}
	f.expiry = f.now.Add(ttl)
	return nil
}

func (f *fakeLease) Release(ctx context.Context, holder string, epoch int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.holder != holder || f.heldEpoch != epoch {
		return nil
	}
	f.holder = ""
	return nil
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
