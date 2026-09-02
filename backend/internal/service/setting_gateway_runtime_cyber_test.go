package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type cyberRuntimeSettingRepo struct {
	mu          sync.Mutex
	values      map[string]string
	getMultiple int
	readStarted chan struct{}
	releaseRead chan struct{}
	getAllErr   error
}

func (r *cyberRuntimeSettingRepo) Get(context.Context, string) (*Setting, error) {
	return nil, ErrSettingNotFound
}
func (r *cyberRuntimeSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}
func (r *cyberRuntimeSettingRepo) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[key] = value
	return nil
}
func (r *cyberRuntimeSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	r.mu.Lock()
	r.getMultiple++
	snapshot := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			snapshot[key] = value
		}
	}
	started, release := r.readStarted, r.releaseRead
	r.readStarted = nil
	r.releaseRead = nil
	r.mu.Unlock()
	if started != nil {
		close(started)
	}
	if release != nil {
		<-release
	}
	return snapshot, nil
}
func (r *cyberRuntimeSettingRepo) SetMultiple(_ context.Context, settings map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values == nil {
		r.values = make(map[string]string)
	}
	for key, value := range settings {
		r.values[key] = value
	}
	return nil
}
func (r *cyberRuntimeSettingRepo) GetAll(context.Context) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getAllErr != nil {
		return nil, r.getAllErr
	}
	out := make(map[string]string, len(r.values))
	for key, value := range r.values {
		out[key] = value
	}
	return out, nil
}

func TestCyberSessionRuntimePartialUpdateSurvivesGetAllFailure(t *testing.T) {
	repo := &cyberRuntimeSettingRepo{values: map[string]string{
		SettingKeyCyberSessionBlockEnabled: "false", SettingKeyCyberSessionBlockTTLSeconds: "75",
	}, getAllErr: errors.New("broad settings reload failed")}
	svc := NewSettingService(repo, nil)
	enabled, ttl := svc.GetCyberSessionBlockRuntime(context.Background())
	require.False(t, enabled)
	require.Equal(t, 75*time.Second, ttl)

	settings := &SystemSettings{CyberSessionBlockEnabled: true}
	updates, err := svc.buildSystemSettingsUpdates(context.Background(), settings)
	require.NoError(t, err)
	omitted := make(OmittedSettingKeys, len(updates)-1)
	for key := range updates {
		if key != SettingKeyCyberSessionBlockEnabled {
			omitted[key] = struct{}{}
		}
	}
	omitted[SettingKeyCyberSessionBlockTTLSeconds] = struct{}{}
	require.NoError(t, svc.UpdateSettingsOmitting(context.Background(), settings, omitted))
	enabled, ttl = svc.GetCyberSessionBlockRuntime(context.Background())
	require.True(t, enabled)
	require.Equal(t, 75*time.Second, ttl)
}
func (r *cyberRuntimeSettingRepo) Delete(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.values, key)
	return nil
}

func TestCyberSessionRuntimeUsesOneSnapshotAndUpdateIsImmediate(t *testing.T) {
	repo := &cyberRuntimeSettingRepo{values: map[string]string{
		SettingKeyCyberSessionBlockEnabled: "true", SettingKeyCyberSessionBlockTTLSeconds: "60",
	}}
	svc := NewSettingService(repo, nil)
	enabled, ttl := svc.GetCyberSessionBlockRuntime(context.Background())
	require.True(t, enabled)
	require.Equal(t, time.Minute, ttl)
	require.Equal(t, 1, repo.getMultiple)

	require.NoError(t, svc.UpdateSettings(context.Background(), &SystemSettings{
		CyberSessionBlockEnabled: false, CyberSessionBlockTTLSeconds: 120,
	}))
	enabled, ttl = svc.GetCyberSessionBlockRuntime(context.Background())
	require.False(t, enabled)
	require.Equal(t, 2*time.Minute, ttl)
	require.Equal(t, 1, repo.getMultiple, "successful update must refresh the same-process snapshot without a DB read")
}

func TestCyberSessionRuntimeConcurrentRefreshCannotOverwriteUpdate(t *testing.T) {
	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	repo := &cyberRuntimeSettingRepo{
		values: map[string]string{
			SettingKeyCyberSessionBlockEnabled: "true", SettingKeyCyberSessionBlockTTLSeconds: "60",
		},
		readStarted: readStarted,
		releaseRead: releaseRead,
	}
	svc := NewSettingService(repo, nil)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		svc.GetCyberSessionBlockRuntime(context.Background())
	}()
	<-readStarted

	updateDone := make(chan error, 1)
	go func() {
		updateDone <- svc.UpdateSettings(context.Background(), &SystemSettings{
			CyberSessionBlockEnabled: false, CyberSessionBlockTTLSeconds: 180,
		})
	}()
	close(releaseRead)
	<-readDone
	require.NoError(t, <-updateDone)
	enabled, ttl := svc.GetCyberSessionBlockRuntime(context.Background())
	require.False(t, enabled)
	require.Equal(t, 3*time.Minute, ttl)
}
