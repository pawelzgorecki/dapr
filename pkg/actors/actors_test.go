/*
Copyright 2021 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package actors

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	kclock "k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/dapr/components-contrib/state"
	"github.com/dapr/dapr/pkg/actors/internal"
	"github.com/dapr/dapr/pkg/apis/resiliency/v1alpha1"
	"github.com/dapr/dapr/pkg/channel"
	"github.com/dapr/dapr/pkg/config"
	"github.com/dapr/dapr/pkg/health"
	invokev1 "github.com/dapr/dapr/pkg/messaging/v1"
	"github.com/dapr/dapr/pkg/modes"
	internalsv1pb "github.com/dapr/dapr/pkg/proto/internals/v1"
	runtimev1pb "github.com/dapr/dapr/pkg/proto/runtime/v1"
	"github.com/dapr/dapr/pkg/resiliency"
	"github.com/dapr/dapr/pkg/runtime/compstore"
	daprt "github.com/dapr/dapr/pkg/testing"
	"github.com/dapr/kit/ptr"
)

const (
	TestAppID                       = "fakeAppID"
	TestKeyName                     = "key0"
	TestPodName                     = "testPodName"
	TestActorMetadataPartitionCount = 3
)

var DefaultAppConfig = config.ApplicationConfig{
	Entities:                   []string{"1", "reentrantActor"},
	ActorIdleTimeout:           "1s",
	ActorScanInterval:          "2s",
	DrainOngoingCallTimeout:    "3s",
	DrainRebalancedActors:      true,
	Reentrancy:                 config.ReentrancyConfig{},
	RemindersStoragePartitions: 0,
}

var startOfTime = time.Date(2022, 1, 1, 12, 0, 0, 0, time.UTC)

// testRequest is the request object that encapsulates the `data` field of a request.
type testRequest struct {
	Data any `json:"data"`
}

type mockAppChannel struct {
	channel.AppChannel
	requestC chan testRequest
}

var testResiliency = &v1alpha1.Resiliency{
	Spec: v1alpha1.ResiliencySpec{
		Policies: v1alpha1.Policies{
			Retries: map[string]v1alpha1.Retry{
				"singleRetry": {
					MaxRetries:  ptr.Of(1),
					MaxInterval: "100ms",
					Policy:      "constant",
					Duration:    "10ms",
				},
			},
			Timeouts: map[string]string{
				"fast": "100ms",
			},
		},
		Targets: v1alpha1.Targets{
			Actors: map[string]v1alpha1.ActorPolicyNames{
				"failingActorType": {
					Timeout: "fast",
				},
			},
			Components: map[string]v1alpha1.ComponentPolicyNames{
				"failStore": {
					Outbound: v1alpha1.PolicyNames{
						Retry:   "singleRetry",
						Timeout: "fast",
					},
				},
			},
		},
	},
}

func (m *mockAppChannel) InvokeMethod(ctx context.Context, req *invokev1.InvokeMethodRequest, appID string) (*invokev1.InvokeMethodResponse, error) {
	if m.requestC != nil {
		var request testRequest
		err := json.NewDecoder(req.RawData()).Decode(&request)
		if err == nil {
			m.requestC <- request
		}
	}

	return invokev1.NewInvokeMethodResponse(200, "OK", nil), nil
}

type reentrantAppChannel struct {
	channel.AppChannel
	nextCall []*invokev1.InvokeMethodRequest
	callLog  []string
	a        *actorsRuntime
}

func (r *reentrantAppChannel) InvokeMethod(ctx context.Context, req *invokev1.InvokeMethodRequest, appID string) (*invokev1.InvokeMethodResponse, error) {
	r.callLog = append(r.callLog, "Entering "+req.Message().Method)
	if len(r.nextCall) > 0 {
		nextReq := r.nextCall[0]
		r.nextCall = r.nextCall[1:]

		if val, ok := req.Metadata()["Dapr-Reentrancy-Id"]; ok {
			nextReq.AddMetadata(map[string][]string{
				"Dapr-Reentrancy-Id": val.Values,
			})
		}
		resp, err := r.a.callLocalActor(context.Background(), nextReq)
		if err != nil {
			return nil, err
		}
		defer resp.Close()
	}
	r.callLog = append(r.callLog, "Exiting "+req.Message().Method)

	return invokev1.NewInvokeMethodResponse(200, "OK", nil), nil
}

type runtimeBuilder struct {
	appChannel     channel.AppChannel
	config         *Config
	actorStore     state.Store
	actorStoreName string
	clock          kclock.WithTicker
}

func (b *runtimeBuilder) buildActorRuntime() *actorsRuntime {
	if b.appChannel == nil {
		b.appChannel = new(mockAppChannel)
	}

	if b.config == nil {
		config := NewConfig(ConfigOpts{
			HostAddress:        "",
			AppID:              TestAppID,
			PlacementAddresses: []string{"placement:5050"},
			Port:               0,
			Namespace:          "",
			AppConfig: config.ApplicationConfig{
				Entities: []string{"failingActor"},
			},
			PodName: TestPodName,
		})
		b.config = &config
	}

	store := fakeStore()
	storeName := "actorStore"
	if b.actorStore != nil {
		store = b.actorStore
		storeName = b.actorStoreName
	}

	clock := b.clock
	if clock == nil {
		mc := clocktesting.NewFakeClock(startOfTime)
		clock = mc
	}

	compStore := compstore.New()
	compStore.AddStateStore(storeName, store)
	a := newActorsWithClock(ActorsOpts{
		CompStore:      compStore,
		AppChannel:     b.appChannel,
		Config:         *b.config,
		TracingSpec:    config.TracingSpec{SamplingRate: "1"},
		Resiliency:     resiliency.FromConfigurations(log, testResiliency),
		StateStoreName: storeName,
	}, clock)

	return a.(*actorsRuntime)
}

func newTestActorsRuntimeWithMock(appChannel channel.AppChannel) *actorsRuntime {
	conf := NewConfig(ConfigOpts{
		AppID:              TestAppID,
		PlacementAddresses: []string{"placement:5050"},
		AppConfig: config.ApplicationConfig{
			Entities: []string{"cat", "dog", "actor2"},
		},
	})

	clock := clocktesting.NewFakeClock(startOfTime)

	compStore := compstore.New()
	compStore.AddStateStore("actorStore", fakeStore())
	a := newActorsWithClock(ActorsOpts{
		CompStore:      compStore,
		AppChannel:     appChannel,
		Config:         conf,
		TracingSpec:    config.TracingSpec{SamplingRate: "1"},
		Resiliency:     resiliency.New(log),
		StateStoreName: "actorStore",
		MockPlacement:  NewMockPlacement(TestAppID),
	}, clock)

	return a.(*actorsRuntime)
}

func newTestActorsRuntimeWithMockWithoutPlacement(appChannel channel.AppChannel) *actorsRuntime {
	conf := NewConfig(ConfigOpts{
		AppID:              TestAppID,
		PlacementAddresses: []string{""},
		AppConfig:          config.ApplicationConfig{},
	})

	clock := clocktesting.NewFakeClock(startOfTime)

	a := newActorsWithClock(ActorsOpts{
		CompStore:      compstore.New(),
		AppChannel:     appChannel,
		Config:         conf,
		TracingSpec:    config.TracingSpec{SamplingRate: "1"},
		Resiliency:     resiliency.New(log),
		StateStoreName: "actorStore",
	}, clock)

	return a.(*actorsRuntime)
}

func newTestActorsRuntimeWithMockAndNoStore(appChannel channel.AppChannel) *actorsRuntime {
	conf := NewConfig(ConfigOpts{
		AppID:              TestAppID,
		PlacementAddresses: []string{""},
		AppConfig:          config.ApplicationConfig{},
	})

	clock := clocktesting.NewFakeClock(startOfTime)

	a := newActorsWithClock(ActorsOpts{
		CompStore:      compstore.New(),
		AppChannel:     appChannel,
		Config:         conf,
		TracingSpec:    config.TracingSpec{SamplingRate: "1"},
		Resiliency:     resiliency.New(log),
		StateStoreName: "actorStore",
	}, clock)

	return a.(*actorsRuntime)
}

func newTestActorsRuntimeWithoutStore() *actorsRuntime {
	appChannel := new(mockAppChannel)

	return newTestActorsRuntimeWithMockAndNoStore(appChannel)
}

func newTestActorsRuntime() *actorsRuntime {
	appChannel := new(mockAppChannel)

	return newTestActorsRuntimeWithMock(appChannel)
}

func newTestActorsRuntimeWithoutPlacement() *actorsRuntime {
	appChannel := new(mockAppChannel)

	return newTestActorsRuntimeWithMockWithoutPlacement(appChannel)
}

func getTestActorTypeAndID() (string, string) {
	return "cat", "e485d5de-de48-45ab-816e-6cc700d18ace"
}

func fakeStore() state.Store {
	return daprt.NewFakeStateStore()
}

func fakeCallAndActivateActor(actors *actorsRuntime, actorType, actorID string, clock kclock.WithTicker) {
	actorKey := constructCompositeKey(actorType, actorID)
	act := newActor(actorType, actorID, &reentrancyStackDepth, actors.actorsConfig.GetIdleTimeoutForType(actorType), clock)
	actors.actorsTable.LoadOrStore(actorKey, act)
}

func deactivateActorWithDuration(testActorsRuntime *actorsRuntime, actorType, actorID string) <-chan struct{} {
	fakeCallAndActivateActor(testActorsRuntime, actorType, actorID, testActorsRuntime.clock)

	ch := make(chan struct{}, 1)
	go testActorsRuntime.deactivationTicker(testActorsRuntime.actorsConfig, func(at, aid string) error {
		if actorType == at {
			testActorsRuntime.removeActorFromTable(at, aid)
			ch <- struct{}{}
		}
		return nil
	})
	return ch
}

func assertTestSignal(t *testing.T, clock *clocktesting.FakeClock, ch <-chan struct{}) {
	t.Helper()

	end := clock.Now().Add(700 * time.Millisecond)

	for {
		select {
		case <-ch:
			// all good
			return
		default:
		}

		if clock.Now().After(end) {
			require.Fail(t, "did not receive signal in 700ms")
		}

		// The signal is sent in a background goroutine, so we need to use a wall
		// clock here
		time.Sleep(time.Millisecond * 5)
		advanceTickers(t, clock, time.Millisecond*10)
	}
}

func assertNoTestSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()

	// The signal is sent in a background goroutine, so we need to use a wall clock here
	select {
	case <-ch:
		t.Fatalf("received unexpected signal")
	case <-time.After(500 * time.Millisecond):
		// all good
	}
}

// Makes tickers advance
func advanceTickers(t *testing.T, clock *clocktesting.FakeClock, step time.Duration) {
	t.Helper()

	// Wait for the clock to have tickers before stepping, since they are likely
	// being created in another go routine to this test.
	require.Eventually(t, func() bool {
		return clock.HasWaiters()
	}, 2*time.Second, 5*time.Millisecond, "ticker in program not created in time")
	clock.Step(step)
}

func TestDeactivationTicker(t *testing.T) {
	t.Run("actor is deactivated", func(t *testing.T) {
		testActorsRuntime := newTestActorsRuntime()
		defer testActorsRuntime.Close()
		clock := testActorsRuntime.clock.(*clocktesting.FakeClock)

		actorType, actorID := getTestActorTypeAndID()
		actorKey := constructCompositeKey(actorType, actorID)

		testActorsRuntime.actorsConfig.ActorIdleTimeout = time.Second * 2
		testActorsRuntime.actorsConfig.ActorDeactivationScanInterval = time.Second * 1

		ch := deactivateActorWithDuration(testActorsRuntime, actorType, actorID)

		_, exists := testActorsRuntime.actorsTable.Load(actorKey)
		assert.True(t, exists)

		advanceTickers(t, clock, time.Second*3)

		assertTestSignal(t, clock, ch)

		_, exists = testActorsRuntime.actorsTable.Load(actorKey)
		assert.False(t, exists)
	})

	t.Run("actor is not deactivated", func(t *testing.T) {
		testActorsRuntime := newTestActorsRuntime()
		defer testActorsRuntime.Close()
		clock := testActorsRuntime.clock.(*clocktesting.FakeClock)

		actorType, actorID := getTestActorTypeAndID()
		actorKey := constructCompositeKey(actorType, actorID)

		testActorsRuntime.actorsConfig.ActorIdleTimeout = time.Second * 5
		testActorsRuntime.actorsConfig.ActorDeactivationScanInterval = time.Second * 1

		ch := deactivateActorWithDuration(testActorsRuntime, actorType, actorID)

		_, exists := testActorsRuntime.actorsTable.Load(actorKey)
		assert.True(t, exists)

		advanceTickers(t, clock, time.Second*3)
		assertNoTestSignal(t, ch)

		_, exists = testActorsRuntime.actorsTable.Load(actorKey)
		assert.True(t, exists)
	})

	t.Run("per-actor timeout", func(t *testing.T) {
		testActorsRuntime := newTestActorsRuntime()
		defer testActorsRuntime.Close()
		clock := testActorsRuntime.clock.(*clocktesting.FakeClock)

		firstType := "a"
		secondType := "b"
		actorID := "1"

		testActorsRuntime.actorsConfig.EntityConfigs[firstType] = internal.EntityConfig{Entities: []string{firstType}, ActorIdleTimeout: time.Second * 2}
		testActorsRuntime.actorsConfig.EntityConfigs[secondType] = internal.EntityConfig{Entities: []string{secondType}, ActorIdleTimeout: time.Second * 5}
		testActorsRuntime.actorsConfig.ActorDeactivationScanInterval = time.Second * 1

		ch1 := deactivateActorWithDuration(testActorsRuntime, firstType, actorID)
		ch2 := deactivateActorWithDuration(testActorsRuntime, secondType, actorID)

		advanceTickers(t, clock, time.Second*2)
		assertTestSignal(t, clock, ch1)
		assertNoTestSignal(t, ch2)

		_, exists := testActorsRuntime.actorsTable.Load(constructCompositeKey(firstType, actorID))
		assert.False(t, exists)

		_, exists = testActorsRuntime.actorsTable.Load(constructCompositeKey(secondType, actorID))
		assert.True(t, exists)
	})
}

func TestTimerExecution(t *testing.T) {
	testActorsRuntime := newTestActorsRuntime()
	defer testActorsRuntime.Close()

	actorType, actorID := getTestActorTypeAndID()
	fakeCallAndActivateActor(testActorsRuntime, actorType, actorID, testActorsRuntime.clock)

	period, _ := internal.NewReminderPeriod("2s")
	err := testActorsRuntime.doExecuteReminderOrTimer(context.Background(), &internal.Reminder{
		ActorType:      actorType,
		ActorID:        actorID,
		Name:           "timer1",
		Period:         period,
		RegisteredTime: testActorsRuntime.clock.Now().Add(2 * time.Second),
		DueTime:        "2s",
		Callback:       "callback",
		Data:           json.RawMessage(`"data"`),
	}, true)
	assert.NoError(t, err)
}

func TestReminderExecution(t *testing.T) {
	testActorsRuntime := newTestActorsRuntime()
	defer testActorsRuntime.Close()

	actorType, actorID := getTestActorTypeAndID()
	fakeCallAndActivateActor(testActorsRuntime, actorType, actorID, testActorsRuntime.clock)

	period, _ := internal.NewReminderPeriod("2s")
	err := testActorsRuntime.doExecuteReminderOrTimer(context.Background(), &internal.Reminder{
		ActorType:      actorType,
		ActorID:        actorID,
		RegisteredTime: time.Now().Add(2 * time.Second),
		Period:         period,
		Name:           "reminder1",
		Data:           json.RawMessage(`"data"`),
	}, false)
	assert.NoError(t, err)
}

func TestConstructActorStateKey(t *testing.T) {
	delim := "||"
	testActorsRuntime := newTestActorsRuntime()
	defer testActorsRuntime.Close()

	actorType, actorID := getTestActorTypeAndID()
	expected := strings.Join([]string{TestAppID, actorType, actorID, TestKeyName}, delim)

	// act
	stateKey := testActorsRuntime.constructActorStateKey(constructCompositeKey(actorType, actorID), TestKeyName)

	// assert
	assert.Equal(t, expected, stateKey)

	// Check split
	keys := strings.Split(stateKey, delim)
	assert.Equal(t, 4, len(keys))
	assert.Equal(t, TestAppID, keys[0])
	assert.Equal(t, actorType, keys[1])
	assert.Equal(t, actorID, keys[2])
	assert.Equal(t, TestKeyName, keys[3])
}

func TestGetState(t *testing.T) {
	testActorsRuntime := newTestActorsRuntime()
	defer testActorsRuntime.Close()

	actorType, actorID := getTestActorTypeAndID()
	ctx := context.Background()
	fakeData := strconv.Quote("fakeData")

	var val any
	json.Unmarshal([]byte(fakeData), &val)

	fakeCallAndActivateActor(testActorsRuntime, actorType, actorID, testActorsRuntime.clock)

	err := testActorsRuntime.TransactionalStateOperation(ctx, &TransactionalRequest{
		ActorType: actorType,
		ActorID:   actorID,
		Operations: []TransactionalOperation{
			{
				Operation: Upsert,
				Request: TransactionalUpsert{
					Key:   TestKeyName,
					Value: val,
				},
			},
		},
	})
	require.NoError(t, err)

	// act
	response, err := testActorsRuntime.GetState(ctx, &GetStateRequest{
		ActorID:   actorID,
		ActorType: actorType,
		Key:       TestKeyName,
	})

	// assert
	require.NoError(t, err)
	assert.Equal(t, fakeData, string(response.Data))
}

func TestGetBulkState(t *testing.T) {
	testActorsRuntime := newTestActorsRuntime()
	defer testActorsRuntime.Close()

	actorType, actorID := getTestActorTypeAndID()
	ctx := context.Background()
	fakeData := strconv.Quote("fakeData")

	var val any
	json.Unmarshal([]byte(fakeData), &val)

	fakeCallAndActivateActor(testActorsRuntime, actorType, actorID, testActorsRuntime.clock)

	err := testActorsRuntime.TransactionalStateOperation(ctx, &TransactionalRequest{
		ActorType: actorType,
		ActorID:   actorID,
		Operations: []TransactionalOperation{
			{
				Operation: Upsert,
				Request: TransactionalUpsert{
					Key:   "key1",
					Value: val,
				},
			},
			{
				Operation: Upsert,
				Request: TransactionalUpsert{
					Key:   "key2",
					Value: val,
				},
			},
		},
	})
	require.NoError(t, err)

	// act
	response, err := testActorsRuntime.GetBulkState(ctx, &GetBulkStateRequest{
		ActorID:   actorID,
		ActorType: actorType,
		Keys:      []string{"key1", "key2"},
	})

	// assert
	require.NoError(t, err)
	require.Len(t, response, 2)
	assert.Equal(t, fakeData, string(response["key1"]))
	assert.Equal(t, fakeData, string(response["key2"]))
}

func TestDeleteState(t *testing.T) {
	testActorsRuntime := newTestActorsRuntime()
	defer testActorsRuntime.Close()

	actorType, actorID := getTestActorTypeAndID()
	ctx := context.Background()
	fakeData := strconv.Quote("fakeData")

	var val any
	json.Unmarshal([]byte(fakeData), &val)

	fakeCallAndActivateActor(testActorsRuntime, actorType, actorID, testActorsRuntime.clock)

	// insert state
	err := testActorsRuntime.TransactionalStateOperation(ctx, &TransactionalRequest{
		ActorType: actorType,
		ActorID:   actorID,
		Operations: []TransactionalOperation{
			{
				Operation: Upsert,
				Request: TransactionalUpsert{
					Key:   TestKeyName,
					Value: val,
				},
			},
		},
	})
	require.NoError(t, err)

	// save state
	response, err := testActorsRuntime.GetState(ctx, &GetStateRequest{
		ActorID:   actorID,
		ActorType: actorType,
		Key:       TestKeyName,
	})

	// make sure that state is stored.
	require.NoError(t, err)
	assert.Equal(t, fakeData, string(response.Data))

	// delete state
	err = testActorsRuntime.TransactionalStateOperation(ctx, &TransactionalRequest{
		ActorType: actorType,
		ActorID:   actorID,
		Operations: []TransactionalOperation{
			{
				Operation: Delete,
				Request: TransactionalDelete{
					Key: TestKeyName,
				},
			},
		},
	})
	require.NoError(t, err)

	// act
	response, err = testActorsRuntime.GetState(ctx, &GetStateRequest{
		ActorID:   actorID,
		ActorType: actorType,
		Key:       TestKeyName,
	})

	// assert
	assert.NoError(t, err)
	assert.Nilf(t, response.Data, "expected nil, but got %s", string(response.Data))
}

func TestTransactionalOperation(t *testing.T) {
	t.Run("test upsert operations", func(t *testing.T) {
		op := TransactionalOperation{
			Operation: Upsert,
			Request: TransactionalUpsert{
				Key:   TestKeyName,
				Value: "respiri piano per non far rumore",
			},
		}
		res, err := op.StateOperation("base||", StateOperationOpts{})
		require.NoError(t, err)
		require.Equal(t, state.OperationUpsert, res.Operation())

		// Uses a pointer
		op = TransactionalOperation{
			Operation: Upsert,
			Request: &TransactionalUpsert{
				Key:   TestKeyName,
				Value: "respiri piano per non far rumore",
			},
		}
		res, err = op.StateOperation("base||", StateOperationOpts{})
		require.NoError(t, err)
		require.Equal(t, state.OperationUpsert, res.Operation())

		// Missing key
		op = TransactionalOperation{
			Operation: Upsert,
			Request:   &TransactionalUpsert{},
		}
		_, err = op.StateOperation("base||", StateOperationOpts{})
		require.Error(t, err)
	})

	t.Run("test delete operations", func(t *testing.T) {
		op := TransactionalOperation{
			Operation: Delete,
			Request: TransactionalDelete{
				Key: TestKeyName,
			},
		}
		res, err := op.StateOperation("base||", StateOperationOpts{})
		require.NoError(t, err)
		require.Equal(t, state.OperationDelete, res.Operation())

		// Uses a pointer
		op = TransactionalOperation{
			Operation: Delete,
			Request: &TransactionalDelete{
				Key: TestKeyName,
			},
		}
		res, err = op.StateOperation("base||", StateOperationOpts{})
		require.NoError(t, err)
		require.Equal(t, state.OperationDelete, res.Operation())

		// Missing key
		op = TransactionalOperation{
			Operation: Delete,
			Request:   &TransactionalDelete{},
		}
		_, err = op.StateOperation("base||", StateOperationOpts{})
		require.Error(t, err)
	})

	t.Run("error on mismatched request and operation", func(t *testing.T) {
		op := TransactionalOperation{
			Operation: Upsert,
			Request: TransactionalDelete{
				Key: TestKeyName,
			},
		}
		_, err := op.StateOperation("base||", StateOperationOpts{})
		require.Error(t, err)

		op = TransactionalOperation{
			Operation: Delete,
			Request: TransactionalUpsert{
				Key: TestKeyName,
			},
		}
		_, err = op.StateOperation("base||", StateOperationOpts{})
		require.Error(t, err)
	})

	t.Run("request as map", func(t *testing.T) {
		op := TransactionalOperation{
			Operation: Upsert,
			Request: map[string]any{
				"key": TestKeyName,
			},
		}
		resI, err := op.StateOperation("base||", StateOperationOpts{})
		require.NoError(t, err)

		res, ok := resI.(state.SetRequest)
		require.True(t, ok)
		assert.Equal(t, "base||"+TestKeyName, res.Key)
	})

	t.Run("error if ttlInSeconds and actor state TTL not enabled", func(t *testing.T) {
		op := TransactionalOperation{
			Operation: Upsert,
			Request: map[string]any{
				"key":      TestKeyName,
				"metadata": map[string]string{"ttlInSeconds": "1"},
			},
		}
		_, err := op.StateOperation("base||", StateOperationOpts{
			StateTTLEnabled: false,
		})
		assert.ErrorContains(t, err, `ttlInSeconds is not supported without the "ActorStateTTL" feature enabled`)

		resI, err := op.StateOperation("base||", StateOperationOpts{
			StateTTLEnabled: true,
		})
		assert.NoError(t, err)

		res, ok := resI.(state.SetRequest)
		require.True(t, ok)
		assert.Equal(t, "base||"+TestKeyName, res.Key)
	})
}

func TestCallLocalActor(t *testing.T) {
	const (
		testActorType = "pet"
		testActorID   = "dog"
		testMethod    = "bite"
	)

	req := invokev1.NewInvokeMethodRequest(testMethod).WithActor(testActorType, testActorID)
	defer req.Close()

	t.Run("invoke actor successfully", func(t *testing.T) {
		testActorsRuntime := newTestActorsRuntime()
		defer testActorsRuntime.Close()

		resp, err := testActorsRuntime.callLocalActor(context.Background(), req)
		assert.NoError(t, err)
		assert.NotNil(t, resp)
		defer resp.Close()
	})

	t.Run("actor is already disposed", func(t *testing.T) {
		// arrange
		testActorsRuntime := newTestActorsRuntime()
		defer testActorsRuntime.Close()

		actorKey := constructCompositeKey(testActorType, testActorID)
		act := newActor(testActorType, testActorID, &reentrancyStackDepth, 2*time.Second, testActorsRuntime.clock)

		// add test actor
		testActorsRuntime.actorsTable.LoadOrStore(actorKey, act)
		act.lock(nil)
		assert.True(t, act.isBusy())

		// get dispose channel for test actor
		ch := act.channel()
		act.unlock()

		_, closed := <-ch
		assert.False(t, closed, "dispose channel must be closed after unlock")

		// act
		resp, err := testActorsRuntime.callLocalActor(context.Background(), req)

		// assert
		s, _ := status.FromError(err)
		assert.Equal(t, codes.ResourceExhausted, s.Code())
		assert.Nil(t, resp)
	})
}

func TestTransactionalState(t *testing.T) {
	ctx := context.Background()
	t.Run("Single set request succeeds", func(t *testing.T) {
		testActorsRuntime := newTestActorsRuntime()
		defer testActorsRuntime.Close()

		actorType, actorID := getTestActorTypeAndID()

		fakeCallAndActivateActor(testActorsRuntime, actorType, actorID, testActorsRuntime.clock)

		err := testActorsRuntime.TransactionalStateOperation(ctx, &TransactionalRequest{
			ActorType: actorType,
			ActorID:   actorID,
			Operations: []TransactionalOperation{
				{
					Operation: Upsert,
					Request: TransactionalUpsert{
						Key:   "key1",
						Value: "fakeData",
					},
				},
			},
		})
		assert.NoError(t, err)
	})

	t.Run("Multiple requests succeeds", func(t *testing.T) {
		testActorsRuntime := newTestActorsRuntime()
		defer testActorsRuntime.Close()

		actorType, actorID := getTestActorTypeAndID()

		fakeCallAndActivateActor(testActorsRuntime, actorType, actorID, testActorsRuntime.clock)

		err := testActorsRuntime.TransactionalStateOperation(ctx, &TransactionalRequest{
			ActorType: actorType,
			ActorID:   actorID,
			Operations: []TransactionalOperation{
				{
					Operation: Upsert,
					Request: TransactionalUpsert{
						Key:   "key1",
						Value: "fakeData",
					},
				},
				{
					Operation: Delete,
					Request: TransactionalDelete{
						Key: "key1",
					},
				},
			},
		})
		assert.NoError(t, err)
	})

	t.Run("Too many requests fail", func(t *testing.T) {
		testActorsRuntime := newTestActorsRuntime()
		defer testActorsRuntime.Close()

		store, err := testActorsRuntime.stateStore()
		require.NoError(t, err)
		fakeStore, ok := store.(*daprt.FakeStateStore)
		require.True(t, ok)
		fakeStore.MaxOperations = 10

		actorType, actorID := getTestActorTypeAndID()

		fakeCallAndActivateActor(testActorsRuntime, actorType, actorID, testActorsRuntime.clock)

		ops := make([]TransactionalOperation, 20)
		for i := 0; i < 20; i++ {
			ops[i] = TransactionalOperation{
				Operation: Upsert,
				Request: TransactionalUpsert{
					Key:   fmt.Sprintf("key%d", i),
					Value: "hello",
				},
			}
		}
		err = testActorsRuntime.TransactionalStateOperation(ctx, &TransactionalRequest{
			ActorType:  actorType,
			ActorID:    actorID,
			Operations: ops,
		})
		assert.Error(t, err)
		assert.ErrorIs(t, err, ErrTransactionsTooManyOperations)
	})

	t.Run("Wrong request body - should fail", func(t *testing.T) {
		testActorsRuntime := newTestActorsRuntime()
		defer testActorsRuntime.Close()

		actorType, actorID := getTestActorTypeAndID()

		fakeCallAndActivateActor(testActorsRuntime, actorType, actorID, testActorsRuntime.clock)

		err := testActorsRuntime.TransactionalStateOperation(ctx, &TransactionalRequest{
			ActorType: actorType,
			ActorID:   actorID,
			Operations: []TransactionalOperation{
				{
					Operation: Upsert,
					Request:   "wrongBody",
				},
			},
		})
		assert.NotNil(t, err)
	})

	t.Run("Unsupported operation type - should fail", func(t *testing.T) {
		testActorsRuntime := newTestActorsRuntime()
		actorType, actorID := getTestActorTypeAndID()

		fakeCallAndActivateActor(testActorsRuntime, actorType, actorID, testActorsRuntime.clock)

		err := testActorsRuntime.TransactionalStateOperation(ctx, &TransactionalRequest{
			ActorType: actorType,
			ActorID:   actorID,
			Operations: []TransactionalOperation{
				{
					Operation: "Wrong",
					Request:   "wrongBody",
				},
			},
		})
		assert.EqualError(t, err, "operation type Wrong not supported")
	})
}

func TestGetOrCreateActor(t *testing.T) {
	const testActorType = "fakeActor"
	testActorsRuntime := newTestActorsRuntime()
	defer testActorsRuntime.Close()

	t.Run("create new key", func(t *testing.T) {
		act := testActorsRuntime.getOrCreateActor(&internalsv1pb.Actor{
			ActorType: testActorType,
			ActorId:   "id-1",
		})
		assert.NotNil(t, act)
	})

	t.Run("try to create the same key", func(t *testing.T) {
		oldActor := testActorsRuntime.getOrCreateActor(&internalsv1pb.Actor{
			ActorType: testActorType,
			ActorId:   "id-2",
		})
		assert.NotNil(t, oldActor)
		newActor := testActorsRuntime.getOrCreateActor(&internalsv1pb.Actor{
			ActorType: testActorType,
			ActorId:   "id-2",
		})
		assert.Same(t, oldActor, newActor, "should not create new actor")
	})
}

func TestActiveActorsCount(t *testing.T) {
	ctx := context.Background()
	t.Run("Actors Count", func(t *testing.T) {
		expectedCounts := []*runtimev1pb.ActiveActorsCount{{Type: "cat", Count: 2}, {Type: "dog", Count: 1}}

		testActorsRuntime := newTestActorsRuntime()
		testActorsRuntime.actorsConfig.Config.HostedActorTypes = internal.NewHostedActors([]string{"cat", "dog"})
		defer testActorsRuntime.Close()

		fakeCallAndActivateActor(testActorsRuntime, "cat", "abcd", testActorsRuntime.clock)
		fakeCallAndActivateActor(testActorsRuntime, "cat", "xyz", testActorsRuntime.clock)
		fakeCallAndActivateActor(testActorsRuntime, "dog", "xyz", testActorsRuntime.clock)

		actualCounts := testActorsRuntime.getActiveActorsCount(ctx)
		assert.ElementsMatch(t, expectedCounts, actualCounts)
	})

	t.Run("Actors Count empty", func(t *testing.T) {
		expectedCounts := []*runtimev1pb.ActiveActorsCount{}

		testActorsRuntime := newTestActorsRuntime()
		testActorsRuntime.actorsConfig.Config.HostedActorTypes = internal.NewHostedActors([]string{})
		defer testActorsRuntime.Close()

		actualCounts := testActorsRuntime.getActiveActorsCount(ctx)
		assert.Equal(t, expectedCounts, actualCounts)
	})
}

func TestActorsAppHealthCheck(t *testing.T) {
	testFn := func(testActorsRuntime *actorsRuntime) func(t *testing.T) {
		return func(t *testing.T) {
			defer testActorsRuntime.Close()
			clock := testActorsRuntime.clock.(*clocktesting.FakeClock)

			testActorsRuntime.actorsConfig.Config.HostedActorTypes = internal.NewHostedActors([]string{"actor1"})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			opts := []health.Option{
				health.WithClock(clock),
				health.WithFailureThreshold(1),
				health.WithInterval(1 * time.Second),
				health.WithRequestTimeout(100 * time.Millisecond),
			}
			closingCh := make(chan struct{})
			healthy := atomic.Bool{}
			healthy.Store(true)
			go func() {
				defer close(closingCh)
				for v := range testActorsRuntime.getAppHealthCheckChanWithOptions(ctx, opts...) {
					healthy.Store(v)
				}
			}()

			clock.Step(time.Second)

			assert.Eventually(t, func() bool {
				advanceTickers(t, clock, time.Second)
				return !healthy.Load()
			}, time.Second, time.Microsecond*10)

			// Cancel now which should cause the shutdown
			cancel()
			<-closingCh
		}
	}

	t.Run("with state store", testFn(newTestActorsRuntime()))

	t.Run("without state store", testFn(newTestActorsRuntimeWithoutStore()))

	t.Run("no hosted actors without state store", func(t *testing.T) {
		testActorsRuntime := newTestActorsRuntimeWithoutStore()
		defer testActorsRuntime.Close()
		clock := testActorsRuntime.clock.(*clocktesting.FakeClock)

		testActorsRuntime.actorsConfig.HostedActorTypes = internal.NewHostedActors([]string{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		opts := []health.Option{
			health.WithClock(clock),
			health.WithFailureThreshold(1),
			health.WithInterval(1 * time.Second),
			health.WithRequestTimeout(100 * time.Millisecond),
		}

		closingCh := make(chan struct{})
		healthy := atomic.Bool{}
		healthy.Store(true)
		go func() {
			defer close(closingCh)
			for v := range testActorsRuntime.getAppHealthCheckChanWithOptions(ctx, opts...) {
				healthy.Store(v)
			}
		}()

		clock.Step(2 * time.Second)

		assert.Eventually(t, healthy.Load, time.Second, time.Microsecond*10)

		// Cancel now which should cause the shutdown
		cancel()
		<-closingCh
	})
}

func TestShutdown(t *testing.T) {
	testActorsRuntime := newTestActorsRuntime()

	t.Run("no panic when placement is nil", func(t *testing.T) {
		testActorsRuntime.placement = nil
		testActorsRuntime.Close()
		// No panic
	})
}

func TestConstructCompositeKeyWithThreeArgs(t *testing.T) {
	appID := "myapp"
	actorType := "TestActor"
	actorID := "abc123"

	actorKey := constructCompositeKey(appID, actorType, actorID)

	assert.Equal(t, "myapp||TestActor||abc123", actorKey)
}

func TestHostValidation(t *testing.T) {
	t.Run("kubernetes mode with mTLS, missing namespace", func(t *testing.T) {
		err := ValidateHostEnvironment(true, modes.KubernetesMode, "")
		assert.Error(t, err)
	})

	t.Run("kubernetes mode without mTLS, missing namespace", func(t *testing.T) {
		err := ValidateHostEnvironment(false, modes.KubernetesMode, "")
		assert.NoError(t, err)
	})

	t.Run("kubernetes mode with mTLS and namespace", func(t *testing.T) {
		err := ValidateHostEnvironment(true, modes.KubernetesMode, "default")
		assert.NoError(t, err)
	})

	t.Run("self hosted mode with mTLS, missing namespace", func(t *testing.T) {
		err := ValidateHostEnvironment(true, modes.StandaloneMode, "")
		assert.NoError(t, err)
	})

	t.Run("self hosted mode without mTLS, missing namespace", func(t *testing.T) {
		err := ValidateHostEnvironment(false, modes.StandaloneMode, "")
		assert.NoError(t, err)
	})
}

func TestBasicReentrantActorLocking(t *testing.T) {
	req := invokev1.NewInvokeMethodRequest("first").WithActor("reentrant", "1")
	defer req.Close()
	req2 := invokev1.NewInvokeMethodRequest("second").WithActor("reentrant", "1")
	defer req2.Close()

	appConfig := DefaultAppConfig
	appConfig.Reentrancy = config.ReentrancyConfig{Enabled: true}
	reentrantConfig := NewConfig(ConfigOpts{
		AppID:              TestAppID,
		PlacementAddresses: []string{"placement:5050"},
		AppConfig:          appConfig,
	})
	reentrantAppChannel := new(reentrantAppChannel)
	reentrantAppChannel.nextCall = []*invokev1.InvokeMethodRequest{req2}
	reentrantAppChannel.callLog = []string{}
	builder := runtimeBuilder{
		appChannel: reentrantAppChannel,
		config:     &reentrantConfig,
	}
	testActorsRuntime := builder.buildActorRuntime()
	reentrantAppChannel.a = testActorsRuntime

	resp, err := testActorsRuntime.callLocalActor(context.Background(), req)
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	defer resp.Close()
	assert.Equal(t, []string{
		"Entering actors/reentrant/1/method/first", "Entering actors/reentrant/1/method/second",
		"Exiting actors/reentrant/1/method/second", "Exiting actors/reentrant/1/method/first",
	}, reentrantAppChannel.callLog)
}

func TestReentrantActorLockingOverMultipleActors(t *testing.T) {
	req := invokev1.NewInvokeMethodRequest("first").WithActor("reentrant", "1")
	defer req.Close()
	req2 := invokev1.NewInvokeMethodRequest("second").WithActor("other", "1")
	defer req2.Close()
	req3 := invokev1.NewInvokeMethodRequest("third").WithActor("reentrant", "1")
	defer req3.Close()

	appConfig := DefaultAppConfig
	appConfig.Reentrancy = config.ReentrancyConfig{Enabled: true}
	reentrantConfig := NewConfig(ConfigOpts{
		AppID:              TestAppID,
		PlacementAddresses: []string{"placement:5050"},
		AppConfig:          appConfig,
	})
	reentrantAppChannel := new(reentrantAppChannel)
	reentrantAppChannel.nextCall = []*invokev1.InvokeMethodRequest{req2, req3}
	reentrantAppChannel.callLog = []string{}
	builder := runtimeBuilder{
		appChannel: reentrantAppChannel,
		config:     &reentrantConfig,
	}
	testActorsRuntime := builder.buildActorRuntime()
	reentrantAppChannel.a = testActorsRuntime

	resp, err := testActorsRuntime.callLocalActor(context.Background(), req)
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	defer resp.Close()
	assert.Equal(t, []string{
		"Entering actors/reentrant/1/method/first", "Entering actors/other/1/method/second",
		"Entering actors/reentrant/1/method/third", "Exiting actors/reentrant/1/method/third",
		"Exiting actors/other/1/method/second", "Exiting actors/reentrant/1/method/first",
	}, reentrantAppChannel.callLog)
}

func TestReentrancyStackLimit(t *testing.T) {
	req := invokev1.NewInvokeMethodRequest("first").WithActor("reentrant", "1")
	defer req.Close()

	stackDepth := 0
	appConfig := DefaultAppConfig
	appConfig.Reentrancy = config.ReentrancyConfig{Enabled: true, MaxStackDepth: &stackDepth}
	reentrantConfig := NewConfig(ConfigOpts{
		AppID:              TestAppID,
		PlacementAddresses: []string{"placement:5050"},
		AppConfig:          appConfig,
	})
	reentrantAppChannel := new(reentrantAppChannel)
	reentrantAppChannel.nextCall = []*invokev1.InvokeMethodRequest{}
	reentrantAppChannel.callLog = []string{}
	builder := runtimeBuilder{
		appChannel: reentrantAppChannel,
		config:     &reentrantConfig,
	}
	testActorsRuntime := builder.buildActorRuntime()
	reentrantAppChannel.a = testActorsRuntime

	resp, err := testActorsRuntime.callLocalActor(context.Background(), req)
	assert.Nil(t, resp)
	assert.Error(t, err)
}

func TestReentrancyPerActor(t *testing.T) {
	req := invokev1.NewInvokeMethodRequest("first").WithActor("reentrantActor", "1")
	defer req.Close()
	req2 := invokev1.NewInvokeMethodRequest("second").WithActor("reentrantActor", "1")
	defer req2.Close()

	appConfig := DefaultAppConfig
	appConfig.Reentrancy = config.ReentrancyConfig{Enabled: false}
	appConfig.EntityConfigs = []config.EntityConfig{
		{
			Entities: []string{"reentrantActor"},
			Reentrancy: config.ReentrancyConfig{
				Enabled: true,
			},
		},
	}
	reentrantConfig := NewConfig(ConfigOpts{
		AppID:              TestAppID,
		PlacementAddresses: []string{""},
		AppConfig:          appConfig,
	})
	reentrantAppChannel := new(reentrantAppChannel)
	reentrantAppChannel.nextCall = []*invokev1.InvokeMethodRequest{req2}
	reentrantAppChannel.callLog = []string{}
	builder := runtimeBuilder{
		appChannel: reentrantAppChannel,
		config:     &reentrantConfig,
	}
	testActorsRuntime := builder.buildActorRuntime()
	reentrantAppChannel.a = testActorsRuntime

	resp, err := testActorsRuntime.callLocalActor(context.Background(), req)
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	defer resp.Close()
	assert.Equal(t, []string{
		"Entering actors/reentrantActor/1/method/first", "Entering actors/reentrantActor/1/method/second",
		"Exiting actors/reentrantActor/1/method/second", "Exiting actors/reentrantActor/1/method/first",
	}, reentrantAppChannel.callLog)
}

func TestReentrancyStackLimitPerActor(t *testing.T) {
	req := invokev1.NewInvokeMethodRequest("first").WithActor("reentrantActor", "1")
	defer req.Close()

	stackDepth := 0
	appConfig := DefaultAppConfig
	appConfig.Reentrancy = config.ReentrancyConfig{Enabled: false}
	appConfig.EntityConfigs = []config.EntityConfig{
		{
			Entities: []string{"reentrantActor"},
			Reentrancy: config.ReentrancyConfig{
				Enabled:       true,
				MaxStackDepth: &stackDepth,
			},
		},
	}
	reentrantConfig := NewConfig(ConfigOpts{
		AppID:              TestAppID,
		PlacementAddresses: []string{""},
		AppConfig:          appConfig,
	})
	reentrantAppChannel := new(reentrantAppChannel)
	reentrantAppChannel.nextCall = []*invokev1.InvokeMethodRequest{}
	reentrantAppChannel.callLog = []string{}
	builder := runtimeBuilder{
		appChannel: reentrantAppChannel,
		config:     &reentrantConfig,
	}
	testActorsRuntime := builder.buildActorRuntime()
	reentrantAppChannel.a = testActorsRuntime

	resp, err := testActorsRuntime.callLocalActor(context.Background(), req)
	assert.Nil(t, resp)
	assert.Error(t, err)
}

func TestActorsRuntimeResiliency(t *testing.T) {
	actorType := "failingActor"
	actorID := "failingId"
	failingState := &daprt.FailingStatestore{
		Failure: daprt.NewFailure(
			// Transform the keys into actor format.
			map[string]int{
				constructCompositeKey(TestAppID, actorType, actorID, "failingGetStateKey"): 1,
				constructCompositeKey(TestAppID, actorType, actorID, "failingMultiKey"):    1,
				constructCompositeKey("actors", actorType):                                 1, // Default reminder key.
			},
			map[string]time.Duration{
				constructCompositeKey(TestAppID, actorType, actorID, "timeoutGetStateKey"): time.Second * 10,
				constructCompositeKey(TestAppID, actorType, actorID, "timeoutMultiKey"):    time.Second * 10,
				constructCompositeKey("actors", actorType):                                 time.Second * 10, // Default reminder key.
			},
			map[string]int{},
		),
	}
	failingAppChannel := &daprt.FailingAppChannel{
		Failure: daprt.NewFailure(
			nil,
			map[string]time.Duration{
				"timeoutId": time.Second * 10,
			},
			map[string]int{},
		),
		KeyFunc: func(req *invokev1.InvokeMethodRequest) string {
			return req.Actor().ActorId
		},
	}
	builder := runtimeBuilder{
		appChannel:     failingAppChannel,
		actorStore:     failingState,
		actorStoreName: "failStore",
		// This test is using a real wall clock
		clock: &kclock.RealClock{},
	}
	runtime := builder.buildActorRuntime()

	t.Run("callLocalActor times out with resiliency", func(t *testing.T) {
		req := invokev1.NewInvokeMethodRequest("actorMethod").
			WithActor("failingActorType", "timeoutId").
			WithReplay(true)
		defer req.Close()

		start := time.Now()
		resp, err := runtime.callLocalActor(context.Background(), req)
		end := time.Now()

		assert.Error(t, err)
		assert.Nil(t, resp)
		assert.Equal(t, 1, failingAppChannel.Failure.CallCount("timeoutId"))
		assert.Less(t, end.Sub(start), time.Second*10)
	})

	t.Run("test get state retries with resiliency", func(t *testing.T) {
		req := &GetStateRequest{
			Key:       "failingGetStateKey",
			ActorType: actorType,
			ActorID:   actorID,
		}
		_, err := runtime.GetState(context.Background(), req)

		callKey := constructCompositeKey(TestAppID, actorType, actorID, "failingGetStateKey")
		assert.NoError(t, err)
		assert.Equal(t, 2, failingState.Failure.CallCount(callKey))
	})

	t.Run("test get state times out with resiliency", func(t *testing.T) {
		req := &GetStateRequest{
			Key:       "timeoutGetStateKey",
			ActorType: actorType,
			ActorID:   actorID,
		}
		start := time.Now()
		_, err := runtime.GetState(context.Background(), req)
		end := time.Now()

		callKey := constructCompositeKey(TestAppID, actorType, actorID, "timeoutGetStateKey")
		assert.Error(t, err)
		assert.Equal(t, 2, failingState.Failure.CallCount(callKey))
		assert.Less(t, end.Sub(start), time.Second*10)
	})

	t.Run("test state transaction retries with resiliency", func(t *testing.T) {
		req := &TransactionalRequest{
			Operations: []TransactionalOperation{
				{
					Operation: Delete,
					Request: map[string]string{
						"key": "failingMultiKey",
					},
				},
			},
			ActorType: actorType,
			ActorID:   actorID,
		}

		err := runtime.TransactionalStateOperation(context.Background(), req)

		callKey := constructCompositeKey(TestAppID, actorType, actorID, "failingMultiKey")
		assert.NoError(t, err)
		assert.Equal(t, 2, failingState.Failure.CallCount(callKey))
	})

	t.Run("test state transaction times out with resiliency", func(t *testing.T) {
		req := &TransactionalRequest{
			Operations: []TransactionalOperation{
				{
					Operation: Delete,
					Request: map[string]string{
						"key": "timeoutMultiKey",
					},
				},
			},
			ActorType: actorType,
			ActorID:   actorID,
		}

		start := time.Now()
		err := runtime.TransactionalStateOperation(context.Background(), req)
		end := time.Now()

		callKey := constructCompositeKey(TestAppID, actorType, actorID, "timeoutMultiKey")
		assert.Error(t, err)
		assert.Equal(t, 2, failingState.Failure.CallCount(callKey))
		assert.Less(t, end.Sub(start), time.Second*10)
	})

	t.Run("test get reminders retries and times out with resiliency", func(t *testing.T) {
		_, err := runtime.GetReminder(context.Background(), &GetReminderRequest{
			ActorType: actorType,
			ActorID:   actorID,
		})

		callKey := constructCompositeKey("actors", actorType)
		assert.NoError(t, err)
		assert.Equal(t, 2, failingState.Failure.CallCount(callKey))

		// Key will no longer fail, so now we can check the timeout.
		start := time.Now()
		_, err = runtime.GetReminder(context.Background(), &GetReminderRequest{
			ActorType: actorType,
			ActorID:   actorID,
		})
		end := time.Now()

		assert.Error(t, err)
		assert.Equal(t, 4, failingState.Failure.CallCount(callKey)) // Should be called 2 more times.
		assert.Less(t, end.Sub(start), time.Second*10)
	})
}

func TestPlacementSwitchIsNotTurnedOn(t *testing.T) {
	testActorsRuntime := newTestActorsRuntimeWithoutPlacement()
	defer testActorsRuntime.Close()

	t.Run("placement is empty", func(t *testing.T) {
		assert.Nil(t, testActorsRuntime.placement)
	})

	t.Run("the actor store can not be initialized normally", func(t *testing.T) {
		assert.Empty(t, testActorsRuntime.compStore.ListStateStores())
	})
}
