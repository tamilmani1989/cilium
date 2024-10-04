// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package status

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/testutils"
)

type StatusTestSuite struct {
	config Config
	mutex  lock.Mutex
}

func setUpTest(_ testing.TB) *StatusTestSuite {
	s := &StatusTestSuite{}
	s.mutex.Lock()
	s.config = Config{
		Interval:         10 * time.Millisecond,
		WarningThreshold: 20 * time.Millisecond,
		FailureThreshold: 80 * time.Millisecond,
		StackdumpPath:    "",
	}
	s.mutex.Unlock()

	return s
}

func (s *StatusTestSuite) Config() (c Config) {
	s.mutex.Lock()
	c = s.config
	s.mutex.Unlock()
	return
}

func TestVariableProbeInterval(t *testing.T) {
	s := setUpTest(t)

	var runs, ok atomic.Uint64

	p := []Probe{
		{
			Interval: func(failures int) time.Duration {
				// While failing, retry every millisecond
				if failures > 0 {
					return time.Millisecond
				}

				// Ensure that the regular interval would never retry
				return time.Minute
			},
			Probe: func(ctx context.Context) (interface{}, error) {
				// Let 5 runs fail and then succeed
				if runs.Add(1) < 5 {
					return nil, fmt.Errorf("still failing")
				}

				return nil, nil
			},
			OnStatusUpdate: func(status Status) {
				if status.Data == nil && status.Err == nil {
					ok.Add(1)
				}
			},
		},
	}

	collector := NewCollector(p, s.Config())
	defer collector.Close()

	// wait for 5 probe intervals to occur with 1 millisecond interval
	// until we reach success
	require.NoError(t, testutils.WaitUntil(func() bool {
		return ok.Load() >= 1
	}, 1*time.Second))
}

func TestCollectorFailureTimeout(t *testing.T) {
	s := setUpTest(t)

	var ok atomic.Uint64

	p := []Probe{
		{
			Probe: func(ctx context.Context) (interface{}, error) {
				time.Sleep(s.Config().FailureThreshold * 2)
				return nil, nil
			},
			OnStatusUpdate: func(status Status) {
				if status.StaleWarning && status.Data == nil && status.Err != nil {
					if strings.Contains(status.Err.Error(),
						fmt.Sprintf("within %v seconds", s.Config().FailureThreshold.Seconds())) {

						ok.Add(1)
					}
				}
			},
		},
	}

	collector := NewCollector(p, s.Config())
	defer collector.Close()

	// wait for the failure timeout to kick in
	require.NoError(t, testutils.WaitUntil(func() bool {
		return ok.Load() >= 1
	}, 1*time.Second))
	require.Len(t, collector.GetStaleProbes(), 1)
}

func TestCollectorSuccess(t *testing.T) {
	s := setUpTest(t)

	var ok, errs atomic.Uint64
	err := fmt.Errorf("error")

	p := []Probe{
		{
			Probe: func(ctx context.Context) (interface{}, error) {
				if ok.Load() > 3 {
					return nil, err
				}
				return "testData", nil
			},
			OnStatusUpdate: func(status Status) {
				if !errors.Is(status.Err, err) {
					errs.Add(1)
				}
				if !status.StaleWarning && status.Data != nil && status.Err == nil {
					if s, isString := status.Data.(string); isString && s == "testData" {
						ok.Add(1)
					}
				}
			},
		},
	}

	collector := NewCollector(p, s.Config())
	defer collector.Close()

	// wait for the probe to succeed 3 times and to return the error 3 times
	require.NoError(t, testutils.WaitUntil(func() bool {
		return ok.Load() >= 3 && errs.Load() >= 3
	}, 1*time.Second))
	require.Empty(t, collector.GetStaleProbes())
}

func TestCollectorSuccessAfterTimeout(t *testing.T) {
	s := setUpTest(t)

	var ok, timeout atomic.Uint64

	p := []Probe{
		{
			Probe: func(ctx context.Context) (interface{}, error) {
				if timeout.Load() == 0 {
					time.Sleep(2 * s.Config().FailureThreshold)
				}
				return nil, nil
			},
			OnStatusUpdate: func(status Status) {
				if status.StaleWarning {
					timeout.Add(1)
				} else {
					ok.Add(1)
				}

			},
		},
	}

	collector := NewCollector(p, s.Config())
	defer collector.Close()

	// wait for the probe to timeout (warning and failure) and then to succeed
	require.NoError(t, testutils.WaitUntil(func() bool {
		return timeout.Load() == 1 && ok.Load() > 0
	}, 1*time.Second))
	require.Empty(t, collector.GetStaleProbes())
}
