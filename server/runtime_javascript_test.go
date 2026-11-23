// Copyright 2018 The Nakama Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestJsObjectFreeze(t *testing.T) {
	t.Run("after freeze new global vars cannot be created", func(t *testing.T) {
		observer, _ := observer.New(zap.InfoLevel)
		logger := zap.New(observer)
		config := NewConfig(logger)
		config.Runtime.JsReadOnlyGlobals = true

		r := goja.New()
		freezeGlobalObject(config, r)

		p, _ := goja.Compile("test", `
var k = 'new string';
`, true)

		_, err := r.RunProgram(p)
		if err == nil {
			t.Errorf("should've thrown an error")
		}
		if !strings.Contains(err.Error(), "TypeError: Cannot define global variable 'k', global object is not extensible at test:1:1(0)") {
			t.Errorf("should've thrown an error")
		}
	})

	t.Run("after freeze global vars become immutable", func(t *testing.T) {
		observer, _ := observer.New(zap.InfoLevel)
		logger := zap.New(observer)
		config := NewConfig(logger)
		config.Runtime.JsReadOnlyGlobals = true

		r := goja.New()

		p, _ := goja.Compile("test", `
var m = {foo: 'bar'};
`, true)

		_, err := r.RunProgram(p)
		if err != nil {
			t.Errorf("failed to run script: %s", err.Error())
		}

		freezeGlobalObject(config, r)

		p, _ = goja.Compile("test", `
m.foo = 'baz';
`, true)

		_, err = r.RunProgram(p)
		if err == nil {
			t.Errorf("should've thrown an error")
		}
		if !strings.Contains(err.Error(), "TypeError: Cannot assign to read only property 'foo'") {
			t.Errorf("should've thrown an error")
		}
	})

	t.Run("after freeze newly instanced objects are mutable", func(t *testing.T) {
		observer, _ := observer.New(zap.InfoLevel)
		logger := zap.New(observer)
		config := NewConfig(logger)
		config.Runtime.JsReadOnlyGlobals = true

		r := goja.New()

		p, _ := goja.Compile("test", `
var m = new Map();
`, true)

		_, err := r.RunProgram(p)
		if err != nil {
			t.Error("Failed to run JS script")
		}

		freezeGlobalObject(config, r)

		p, _ = goja.Compile("test", `
m.set('a', 1);
m.get('a');
`, true)

		v, err := r.RunProgram(p)
		if err != nil {
			t.Errorf("failed to run script: %s", err.Error())
		}

		if v.String() != "1" {
			t.Errorf("expected returned value to be '1'")
		}
	})
}

func TestRuntimeJSLeaderboardResetCreateTimeNumber(t *testing.T) {
	modules := map[string]string{
		"index.js": `
function leaderboardReset(ctx, logger, nk, leaderboard, reset) {
	if (typeof leaderboard.createTime !== "number") {
		throw new Error("expected leaderboard.createTime to be number, got " + typeof leaderboard.createTime);
	}
	if (leaderboard.createTime !== 1700000123) {
		throw new Error("expected leaderboard.createTime === 1700000123, got " + leaderboard.createTime);
	}
	if (leaderboard.prevReset !== 1700000200) {
		throw new Error("expected leaderboard.prevReset === 1700000200, got " + leaderboard.prevReset);
	}
	if (leaderboard.nextReset !== 1700000300) {
		throw new Error("expected leaderboard.nextReset === 1700000300, got " + leaderboard.nextReset);
	}
	if (reset !== 1700000250) {
		throw new Error("expected reset === 1700000250, got " + reset);
	}
}

var InitModule = function(ctx, logger, nk, initializer) {
	initializer.registerLeaderboardReset(leaderboardReset);
};
`,
	}

	rt, _, err := runtimeWithModules(t, modules)
	require.NoError(t, err)

	fn := rt.LeaderboardReset()
	require.NotNil(t, fn)

	err = fn(context.Background(), &api.Leaderboard{
		Id:            "weekly-lb",
		Authoritative: true,
		SortOrder:     LeaderboardSortOrderDescending,
		Operator:      api.Operator_BEST,
		PrevReset:     1700000200,
		NextReset:     1700000300,
		Metadata:      `{"season":1}`,
		CreateTime:    &timestamppb.Timestamp{Seconds: 1700000123},
	}, 1700000250)
	require.NoError(t, err)
}

func TestRuntimeJSLeaderboardResetNilCreateTime(t *testing.T) {
	modules := map[string]string{
		"index.js": `
function leaderboardReset(ctx, logger, nk, leaderboard, reset) {
	if (typeof leaderboard.createTime !== "number" || leaderboard.createTime !== 0) {
		throw new Error("expected leaderboard.createTime === 0 when CreateTime is nil, got " + leaderboard.createTime);
	}
}

var InitModule = function(ctx, logger, nk, initializer) {
	initializer.registerLeaderboardReset(leaderboardReset);
};
`,
	}

	rt, _, err := runtimeWithModules(t, modules)
	require.NoError(t, err)

	fn := rt.LeaderboardReset()
	require.NotNil(t, fn)

	err = fn(context.Background(), &api.Leaderboard{
		Id:       "nil-createtime-lb",
		Operator: api.Operator_BEST,
		Metadata: "{}",
	}, 1700000250)
	require.NoError(t, err)
}

func TestRuntimeJSLeaderboardResetScheduledEndToEnd(t *testing.T) {
	ctx := context.Background()
	db := NewDB(t)
	t.Cleanup(func() {
		_ = db.Close()
	})

	lbID := "js-sched-reset-" + uuid.Must(uuid.NewV4()).String()
	storageCollection := "js_leaderboard_reset_e2e"
	storageKey := lbID

	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), "DELETE FROM leaderboard WHERE id = $1", lbID)
		_, _ = db.ExecContext(context.Background(), "DELETE FROM storage WHERE collection = $1 AND key = $2", storageCollection, storageKey)
	})

	dir := t.TempDir()
	jsModule := fmt.Sprintf(`
var targetId = %q;
var storageCollection = %q;
var storageKey = %q;
var handled = false;

function leaderboardReset(ctx, logger, nk, leaderboard, reset) {
	if (leaderboard.id !== targetId || handled) {
		return;
	}
	handled = true;

	nk.storageWrite([{
		collection: storageCollection,
		key: storageKey,
		value: {
			id: leaderboard.id,
			createTimeType: typeof leaderboard.createTime,
			createTime: typeof leaderboard.createTime === "number" ? leaderboard.createTime : -1,
			prevReset: leaderboard.prevReset || 0,
			nextReset: leaderboard.nextReset || 0,
			reset: reset,
			season: leaderboard.metadata.season
		},
		permissionRead: 2,
		permissionWrite: 0
	}]);
}

var InitModule = function(ctx, logger, nk, initializer) {
	initializer.registerLeaderboardReset(leaderboardReset);
	nk.leaderboardCreate(targetId, true, "desc", "best", "* * * * * * *", { season: 3 }, false);
};
`, lbID, storageCollection, storageKey)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.js"), []byte(jsModule), 0o644))

	testCfg := NewConfig(logger)
	testCfg.Runtime.Path = dir

	lbCache := NewLocalLeaderboardCache(ctx, logger, logger, db)
	lbRankCache := NewLocalLeaderboardRankCache(ctx, logger, db, testCfg.GetLeaderboard(), lbCache)
	lbSched := NewLocalLeaderboardScheduler(logger, db, testCfg, lbCache, lbRankCache)
	t.Cleanup(lbSched.Stop)

	sessionRegistry := NewLocalSessionRegistry(metrics)
	tracker := &LocalTracker{sessionRegistry: sessionRegistry}
	statusRegistry := NewLocalStatusRegistry(logger, testCfg, sessionRegistry, protojsonMarshaler)

	rt, _, err := NewRuntime(ctx, logger, logger, db, protojsonMarshaler, protojsonUnmarshaler, testCfg, "", nil, lbCache, lbRankCache, lbSched, sessionRegistry, nil, statusRegistry, nil, nil, tracker, metrics, nil, &DummyMessageRouter{}, storageIdx, nil)
	require.NoError(t, err)

	cachedLb := lbCache.Get(lbID)
	require.NotNil(t, cachedLb)
	require.Greater(t, cachedLb.CreateTime, int64(0))

	lbSched.Start(rt)

	var rawValue string
	require.Eventually(t, func() bool {
		objs, readErr := StorageReadObjects(ctx, logger, db, uuid.Nil, []*api.ReadStorageObjectId{
			{
				Collection: storageCollection,
				Key:        storageKey,
			},
		})
		if readErr != nil || len(objs.GetObjects()) != 1 {
			return false
		}
		rawValue = objs.Objects[0].GetValue()
		return rawValue != ""
	}, 6*time.Second, 50*time.Millisecond, "timed out waiting for LocalLeaderboardScheduler to invoke JS leaderboardReset hook")

	var payload struct {
		ID             string `json:"id"`
		CreateTimeType string `json:"createTimeType"`
		CreateTime     int64  `json:"createTime"`
		PrevReset      int64  `json:"prevReset"`
		NextReset      int64  `json:"nextReset"`
		Reset          int64  `json:"reset"`
		Season         int64  `json:"season"`
	}
	require.NoError(t, json.Unmarshal([]byte(rawValue), &payload))
	require.Equal(t, lbID, payload.ID)
	require.Equal(t, "number", payload.CreateTimeType)
	require.Equal(t, cachedLb.CreateTime, payload.CreateTime)
	require.GreaterOrEqual(t, payload.Reset, cachedLb.CreateTime)
	require.Equal(t, payload.Reset, payload.NextReset)
	require.Equal(t, int64(3), payload.Season)
}
