//go:build !windows

package execution

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIEngineCancellationRemovesChildProcessGroup(t *testing.T) {
	engine := NewGenericCLIEngine("test-model", helperExecutorConfig(t, "spawn-child"))
	require.NoError(t, engine.Initialize(context.Background()))
	t.Cleanup(func() { require.NoError(t, engine.Shutdown(context.Background())) })
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	resp, err := engine.Execute(ctx, &ExecutionRequest{Message: "hello"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 3*time.Second)
	require.NotNil(t, resp)
	data, readErr := os.ReadFile(resp.WorkspaceDir + "/child.pid")
	require.NoError(t, readErr)
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, parseErr)

	require.Eventually(t, func() bool {
		err := syscall.Kill(pid, 0)
		return err == syscall.ESRCH
	}, 3*time.Second, 25*time.Millisecond)
	assert.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH)
}
