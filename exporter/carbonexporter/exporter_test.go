// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package carbonexporter

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/config/confignet"
	"go.opentelemetry.io/collector/exporter/exporterhelper"
	"go.opentelemetry.io/collector/exporter/exportertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	conventions "go.opentelemetry.io/collector/semconv/v1.9.0"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/common/testutil"
)

func TestNewWithDefaultConfig(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	got, err := newCarbonExporter(cfg, exportertest.NewNopCreateSettings())
	assert.NotNil(t, got)
	assert.NoError(t, err)
}

func TestConsumeMetricsNoServer(t *testing.T) {
	exp, err := newCarbonExporter(
		&Config{
			TCPAddr:         confignet.TCPAddr{Endpoint: testutil.GetAvailableLocalAddress(t)},
			TimeoutSettings: exporterhelper.TimeoutSettings{Timeout: 5 * time.Second},
		},
		exportertest.NewNopCreateSettings())
	require.NoError(t, err)
	require.NoError(t, exp.Start(context.Background(), componenttest.NewNopHost()))
	require.Error(t, exp.ConsumeMetrics(context.Background(), generateLargeBatch()))
	require.NoError(t, exp.Shutdown(context.Background()))
}

func TestConsumeMetrics(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping test on windows, see https://github.com/open-telemetry/opentelemetry-collector-contrib/issues/10147")
	}

	tests := []struct {
		name              string
		md                pmetric.Metrics
		numProducers      int
		writesPerProducer int
	}{
		{
			name:              "small_batch",
			md:                generateSmallBatch(),
			numProducers:      1,
			writesPerProducer: 5,
		},
		{
			name:              "large_batch",
			md:                generateLargeBatch(),
			numProducers:      1,
			writesPerProducer: 5,
		},
		{
			name:              "concurrent_small_batch",
			md:                generateSmallBatch(),
			numProducers:      5,
			writesPerProducer: 5,
		},
		{
			name:              "concurrent_large_batch",
			md:                generateLargeBatch(),
			numProducers:      5,
			writesPerProducer: 5,
		},
		{
			name:              "high_concurrency",
			md:                generateLargeBatch(),
			numProducers:      10,
			writesPerProducer: 200,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := testutil.GetAvailableLocalAddress(t)
			cs := newCarbonServer(t, addr)
			// Each metric point will generate one Carbon line, set up the wait
			// for all of them.
			cs.start(t, tt.numProducers*tt.writesPerProducer*tt.md.DataPointCount())

			exp, err := newCarbonExporter(
				&Config{
					TCPAddr:         confignet.TCPAddr{Endpoint: addr},
					TimeoutSettings: exporterhelper.TimeoutSettings{Timeout: 5 * time.Second},
				},
				exportertest.NewNopCreateSettings())
			require.NoError(t, err)
			require.NoError(t, exp.Start(context.Background(), componenttest.NewNopHost()))

			startCh := make(chan struct{})
			var writersWG sync.WaitGroup
			writersWG.Add(tt.numProducers)
			for i := 0; i < tt.numProducers; i++ {
				go func() {
					defer writersWG.Done()
					<-startCh
					for j := 0; j < tt.writesPerProducer; j++ {
						require.NoError(t, exp.ConsumeMetrics(context.Background(), tt.md))
					}
				}()
			}

			// Release all senders.
			close(startCh)
			// Wait for all senders to finish.
			writersWG.Wait()

			assert.NoError(t, exp.Shutdown(context.Background()))
			cs.shutdownAndVerify(t)
		})
	}
}

func generateSmallBatch() pmetric.Metrics {
	metrics := pmetric.NewMetrics()
	m := metrics.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("test_gauge")
	dp := m.SetEmptyGauge().DataPoints().AppendEmpty()
	dp.Attributes().PutStr("k0", "v0")
	dp.Attributes().PutStr("k1", "v1")
	dp.SetTimestamp(pcommon.NewTimestampFromTime(time.Now()))
	dp.SetDoubleValue(123)
	return metrics
}

func generateLargeBatch() pmetric.Metrics {
	ts := time.Now()
	metrics := pmetric.NewMetrics()
	rm := metrics.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr(conventions.AttributeServiceName, "test_carbon")
	ms := rm.ScopeMetrics().AppendEmpty().Metrics()

	for i := 0; i < 1028; i++ {
		m := ms.AppendEmpty()
		m.SetName("test_" + strconv.Itoa(i))
		dp := m.SetEmptyGauge().DataPoints().AppendEmpty()
		dp.Attributes().PutStr("k0", "v0")
		dp.Attributes().PutStr("k1", "v1")
		dp.SetTimestamp(pcommon.NewTimestampFromTime(ts))
		dp.SetIntValue(int64(i))
	}

	return metrics
}

type carbonServer struct {
	ln         *net.TCPListener
	doneServer *atomic.Bool
	wg         sync.WaitGroup
}

func newCarbonServer(t *testing.T, addr string) *carbonServer {
	laddr, err := net.ResolveTCPAddr("tcp", addr)
	require.NoError(t, err)
	ln, err := net.ListenTCP("tcp", laddr)
	require.NoError(t, err)
	return &carbonServer{
		ln:         ln,
		doneServer: &atomic.Bool{},
	}
}

func (cs *carbonServer) start(t *testing.T, numExpectedReq int) {
	cs.wg.Add(numExpectedReq)
	go func() {
		for {
			conn, err := cs.ln.Accept()
			if cs.doneServer.Load() {
				// Close is expected to cause error.
				return
			}
			require.NoError(t, err)
			go func(conn net.Conn) {
				defer func() {
					require.NoError(t, conn.Close())
				}()

				reader := bufio.NewReader(conn)
				for {
					// Actual metric validation is done by other tests, here it
					// is just flow.
					_, err := reader.ReadBytes(byte('\n'))
					if errors.Is(err, io.EOF) {
						return
					}
					require.NoError(t, err)

					cs.wg.Done()
				}
			}(conn)
		}
	}()
	<-time.After(100 * time.Millisecond)
}

func (cs *carbonServer) shutdownAndVerify(t *testing.T) {
	cs.wg.Wait()
	cs.doneServer.Store(true)
	require.NoError(t, cs.ln.Close())
}
