package renditioncleanup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

const (
	testRenditionID = "11111111-1111-4111-8111-111111111111"
	testOtherID     = "22222222-2222-4222-8222-222222222222"
	testPath        = "renditions/aa/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb/11111111-1111-4111-8111-111111111111.avif"
)

type serviceFunc func(context.Context, string, []string, func(context.Context, string, string, int64) (bool, error)) (string, error)

func (f serviceFunc) CleanupNextRendition(ctx context.Context, preferred string, excluded []string, unlink func(context.Context, string, string, int64) (bool, error)) (string, error) {
	return f(ctx, preferred, excluded, unlink)
}

type fakeStore struct {
	started chan struct{}
	release chan struct{}
	calls   int
	err     error
}

func (s *fakeStore) DeleteRendition(context.Context, storage.RenditionKey, storage.DeleteExpectation) (storage.DeleteResult, error) {
	s.calls++
	if s.started != nil {
		close(s.started)
		<-s.release
	}
	return storage.DeleteResult{}, s.err
}

func testOptions(now *time.Time, sleeps *[]time.Duration) Options {
	return Options{
		SweepInterval: time.Second, SweepLimit: 2,
		TransientBackoff: 100 * time.Millisecond, MaxTransientBackoff: 200 * time.Millisecond,
		PoisonCooldown: time.Minute, MaxPoisonCooldown: 2 * time.Minute,
		Now: func() time.Time { return *now },
		Sleep: func(ctx context.Context, delay time.Duration) error {
			*sleeps = append(*sleeps, delay)
			return nil
		},
		Random: func() float64 { return 1 },
	}
}

func TestRunnerBoundsSweeps(t *testing.T) {
	now := time.Unix(100, 0)
	var sleeps []time.Duration
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	service := serviceFunc(func(context.Context, string, []string, func(context.Context, string, string, int64) (bool, error)) (string, error) {
		calls++
		if calls == 4 {
			cancel()
		}
		return testRenditionID, nil
	})
	runner, err := New(service, &fakeStore{}, testOptions(&now, &sleeps), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 4 || !reflect.DeepEqual(sleeps, []time.Duration{time.Second}) {
		t.Fatalf("calls=%d sleeps=%v", calls, sleeps)
	}
}

func TestRunnerRetriesUncertaintyOnSameObjectFirst(t *testing.T) {
	now := time.Unix(100, 0)
	var sleeps []time.Duration
	var preferred []string
	ctx, cancel := context.WithCancel(context.Background())
	service := serviceFunc(func(_ context.Context, id string, _ []string, _ func(context.Context, string, string, int64) (bool, error)) (string, error) {
		preferred = append(preferred, id)
		if len(preferred) == 1 {
			return testRenditionID, storage.ErrOutcomeUncertain
		}
		cancel()
		return testRenditionID, nil
	})
	runner, _ := New(service, &fakeStore{}, testOptions(&now, &sleeps), nil)
	if err := runner.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(preferred, []string{"", testRenditionID}) {
		t.Fatalf("preferred=%v", preferred)
	}
	if len(sleeps) != 1 || sleeps[0] != 100*time.Millisecond {
		t.Fatalf("sleeps=%v", sleeps)
	}
}

func TestRunnerCapsTransientBackoffWithJitter(t *testing.T) {
	now := time.Unix(100, 0)
	var sleeps []time.Duration
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	service := serviceFunc(func(context.Context, string, []string, func(context.Context, string, string, int64) (bool, error)) (string, error) {
		calls++
		if calls == 4 {
			cancel()
		}
		return "", context.DeadlineExceeded
	})
	options := testOptions(&now, &sleeps)
	options.Random = func() float64 { return 0 }
	runner, _ := New(service, &fakeStore{}, options, nil)
	if err := runner.Run(ctx); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, time.Second, 100 * time.Millisecond}
	if !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("sleeps=%v want=%v", sleeps, want)
	}
}

func TestBackoffCapsPoisonCooldownAndInjectedRandom(t *testing.T) {
	now := time.Unix(100, 0)
	var sleeps []time.Duration
	options := testOptions(&now, &sleeps)
	options.Random = func() float64 { return 10 }
	runner, _ := New(serviceFunc(func(context.Context, string, []string, func(context.Context, string, string, int64) (bool, error)) (string, error) {
		return "", nil
	}), &fakeStore{}, options, nil)
	if delay := runner.backoff(time.Minute, 2*time.Minute, 20); delay != 2*time.Minute {
		t.Fatalf("delay=%s", delay)
	}
}

func TestRunnerBoundsTrackedPoisonCandidates(t *testing.T) {
	now := time.Unix(100, 0)
	var sleeps []time.Duration
	runner, _ := New(serviceFunc(func(context.Context, string, []string, func(context.Context, string, string, int64) (bool, error)) (string, error) {
		return "", nil
	}), &fakeStore{}, testOptions(&now, &sleeps), nil)
	for index := range maxTrackedPoisonCandidates + 1 {
		id := string(rune(index + 1))
		runner.deferPoison(id)
	}
	if len(runner.poison) != maxTrackedPoisonCandidates {
		t.Fatalf("tracked poison candidates=%d, want %d", len(runner.poison), maxTrackedPoisonCandidates)
	}
	if _, retained := runner.poison[string(rune(1))]; retained {
		t.Fatal("deterministic oldest candidate was not evicted")
	}
	if _, retained := runner.poison[string(rune(maxTrackedPoisonCandidates+1))]; !retained {
		t.Fatal("new poison candidate was not retained")
	}
}

func TestRunnerMoreThanTrackedPoisonCandidatesReachesHealthySuccessor(t *testing.T) {
	now := time.Unix(100, 0)
	var sleeps []time.Duration
	const poisonCount = maxTrackedPoisonCandidates + 1
	calls := 0
	maxExcluded := 0
	healthySeen := false
	ctx, cancel := context.WithCancel(context.Background())
	service := serviceFunc(func(_ context.Context, _ string, excluded []string, _ func(context.Context, string, string, int64) (bool, error)) (string, error) {
		calls++
		if len(excluded) > maxExcluded {
			maxExcluded = len(excluded)
		}
		if calls <= poisonCount {
			id := fmt.Sprintf("poison-%04d", calls)
			for _, skipped := range excluded {
				if skipped == id {
					t.Fatalf("forward candidate %q was already excluded", id)
				}
			}
			return id, storage.ErrValidation
		}
		healthySeen = true
		cancel()
		return testOtherID, nil
	})
	options := testOptions(&now, &sleeps)
	options.SweepLimit = 50
	runner, err := New(service, &fakeStore{}, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !healthySeen || calls != poisonCount+1 {
		t.Fatalf("healthy successor seen=%t after %d calls, want %d", healthySeen, calls, poisonCount+1)
	}
	if len(runner.poison) != maxTrackedPoisonCandidates || maxExcluded != maxTrackedPoisonCandidates {
		t.Fatalf("tracked poison=%d max exclusions=%d, want %d", len(runner.poison), maxExcluded, maxTrackedPoisonCandidates)
	}
}

func TestRunnerPoisonCooldownDoesNotBlockOtherCandidates(t *testing.T) {
	now := time.Unix(100, 0)
	var sleeps []time.Duration
	var excluded [][]string
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	service := serviceFunc(func(_ context.Context, _ string, skip []string, _ func(context.Context, string, string, int64) (bool, error)) (string, error) {
		excluded = append(excluded, append([]string(nil), skip...))
		calls++
		if calls == 1 {
			return testRenditionID, storage.ErrValidation
		}
		cancel()
		return testOtherID, nil
	})
	runner, _ := New(service, &fakeStore{}, testOptions(&now, &sleeps), nil)
	if err := runner.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(excluded, [][]string{nil, {testRenditionID}}) {
		t.Fatalf("excluded=%v", excluded)
	}
	state := runner.poison[testRenditionID]
	if state.until.Sub(now) != time.Minute {
		t.Fatalf("cooldown=%s", state.until.Sub(now))
	}
}

func TestRunnerTreatsUnclassifiedCandidateFailureAsFatal(t *testing.T) {
	want := errors.New("schema invariant")
	runner, _ := New(serviceFunc(func(context.Context, string, []string, func(context.Context, string, string, int64) (bool, error)) (string, error) {
		return testRenditionID, want
	}), &fakeStore{}, Options{}, nil)
	if err := runner.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("error=%v, want fatal invariant", err)
	}
}

func TestRunnerShutdownBeforeCallDoesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	runner, _ := New(serviceFunc(func(context.Context, string, []string, func(context.Context, string, string, int64) (bool, error)) (string, error) {
		calls++
		return "", nil
	}), &fakeStore{}, Options{}, nil)
	if err := runner.Run(ctx); err != nil || calls != 0 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestRunnerShutdownBeforeUnlinkDoesNotTouchStore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &fakeStore{}
	runner, _ := New(serviceFunc(func(ctx context.Context, _ string, _ []string, unlink func(context.Context, string, string, int64) (bool, error)) (string, error) {
		cancel()
		_, err := unlink(ctx, testRenditionID, testPath, 1)
		return testRenditionID, err
	}), store, Options{}, nil)
	if err := runner.Run(ctx); err != nil || store.calls != 0 {
		t.Fatalf("error=%v store calls=%d", err, store.calls)
	}
}

func TestRunnerShutdownDuringCallWaitsForReturn(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	runner, _ := New(serviceFunc(func(context.Context, string, []string, func(context.Context, string, string, int64) (bool, error)) (string, error) {
		close(started)
		<-release
		return "", context.Canceled
	}), &fakeStore{}, Options{}, nil)
	go func() {
		_ = runner.Run(ctx)
		close(returned)
	}()
	<-started
	cancel()
	select {
	case <-returned:
		t.Fatal("runner returned before in-flight call")
	default:
	}
	close(release)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("runner did not return after in-flight call")
	}
}

func TestUnlinkValidatesIdentityAndWaitsForStore(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	store := &fakeStore{started: started, release: release}
	runner, _ := New(serviceFunc(func(context.Context, string, []string, func(context.Context, string, string, int64) (bool, error)) (string, error) {
		return "", nil
	}), store, Options{}, nil)
	if _, err := runner.unlink(context.Background(), testOtherID, testPath, 1); !errors.Is(err, storage.ErrValidation) {
		t.Fatalf("identity error=%v", err)
	}
	done := make(chan error)
	go func() {
		_, err := runner.unlink(context.Background(), testRenditionID, testPath, 1)
		done <- err
	}()
	<-started
	select {
	case <-done:
		t.Fatal("unlink returned before store")
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUnlinkRejectsTypedKeyIdentityAndSizeMismatchesWithoutDeleting(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(root, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	originalID, err := storage.ParseOriginalID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	targetID, err := storage.ParseJobTargetID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	renditionID, err := storage.ParseRenditionID(testRenditionID)
	if err != nil {
		t.Fatal(err)
	}
	key, err := storage.NewRenditionKey(originalID, targetID, renditionID, storage.RenditionAVIF)
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := storage.ParseAttemptID(testOtherID)
	if err != nil {
		t.Fatal(err)
	}
	temporary, err := store.BeginRendition(context.Background(), key, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("exact payload")
	if _, err := temporary.Write(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := temporary.Publish(context.Background(), storage.Validation{ExpectedSize: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	runner, err := New(serviceFunc(func(context.Context, string, []string, Unlink) (string, error) {
		return "", nil
	}), store, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		id   string
		path string
		size int64
	}{
		{name: "malformed typed key", id: testRenditionID, path: "renditions/not-a-key.avif", size: int64(len(payload))},
		{name: "key and callback ID differ", id: testOtherID, path: key.String(), size: int64(len(payload))},
		{name: "negative expected size", id: testRenditionID, path: key.String(), size: -1},
		{name: "stored size differs", id: testRenditionID, path: key.String(), size: int64(len(payload) + 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := runner.unlink(context.Background(), test.id, test.path, test.size); !errors.Is(err, storage.ErrValidation) {
				t.Fatalf("unlink error = %v, want validation", err)
			}
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(key.String()))); err != nil {
				t.Fatalf("mismatched unlink changed object: %v", err)
			}
		})
	}
}

func TestRunnerConcurrentShutdownDuringUnlinkWaits(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	store := &fakeStore{started: started, release: release}
	ctx, cancel := context.WithCancel(context.Background())
	runner, _ := New(serviceFunc(func(ctx context.Context, _ string, _ []string, unlink func(context.Context, string, string, int64) (bool, error)) (string, error) {
		_, err := unlink(ctx, testRenditionID, testPath, 1)
		return testRenditionID, err
	}), store, Options{}, nil)
	done := make(chan struct{})
	go func() { _ = runner.Run(ctx); close(done) }()
	<-started
	cancel()
	select {
	case <-done:
		t.Fatal("runner returned during unlink")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runner did not wait for unlink")
	}
}
