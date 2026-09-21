package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// WireMock starts a disposable WireMock container and returns its base URL
// for stubbing via StubWireMock. Used by
// testing/integration payment and inventory client tests to exercise real
// HTTP code paths (headers, status handling, timeouts) against a real server,
// instead of mocking the adapters' own methods.
func WireMock(tb testing.TB) string {
	tb.Helper()
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "wiremock/wiremock:3.9.1",
		ExposedPorts: []string{"8080/tcp"},
		WaitingFor:   wait.ForHTTP("/__admin/mappings").WithPort("8080/tcp").WithStartupTimeout(30 * time.Second),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		tb.Fatalf("start wiremock container: %v", err)
	}
	tb.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			tb.Logf("terminate wiremock container: %v", err)
		}
	})

	host, err := container.Host(ctx)
	if err != nil {
		tb.Fatalf("get wiremock host: %v", err)
	}
	port, err := container.MappedPort(ctx, "8080")
	if err != nil {
		tb.Fatalf("get wiremock port: %v", err)
	}
	return fmt.Sprintf("http://%s:%s", host, port.Port())
}

// Stub describes one WireMock stub mapping.
type Stub struct {
	Method         string
	URLPath        string
	Status         int
	Body           string
	RequestBody    string
	RequestHeaders map[string]string
	FixedDelayMs   int
}

// StubWireMock registers a stub against the WireMock admin API.
func StubWireMock(tb testing.TB, baseURL string, s Stub) {
	tb.Helper()

	request := map[string]any{
		"method":  s.Method,
		"urlPath": s.URLPath,
	}
	if s.RequestBody != "" {
		request["bodyPatterns"] = []map[string]any{{"equalToJson": s.RequestBody}}
	}
	if len(s.RequestHeaders) > 0 {
		headers := make(map[string]any, len(s.RequestHeaders))
		for name, value := range s.RequestHeaders {
			headers[name] = map[string]string{"equalTo": value}
		}
		request["headers"] = headers
	}
	mapping := map[string]any{
		"request": request,
		"response": map[string]any{
			"status":                 s.Status,
			"headers":                map[string]string{"Content-Type": "application/json"},
			"jsonBody":               json.RawMessage(s.Body),
			"fixedDelayMilliseconds": s.FixedDelayMs,
		},
	}
	payload, err := json.Marshal(mapping)
	if err != nil {
		tb.Fatalf("marshal wiremock stub: %v", err)
	}

	resp, err := http.Post(baseURL+"/__admin/mappings", "application/json", bytes.NewReader(payload))
	if err != nil {
		tb.Fatalf("register wiremock stub: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			tb.Errorf("close wiremock response: %v", err)
		}
	}()
	if resp.StatusCode >= 300 {
		tb.Fatalf("register wiremock stub: status %d", resp.StatusCode)
	}
}
