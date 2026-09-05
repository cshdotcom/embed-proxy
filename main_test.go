package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// ---------- 配置保留 ----------

func TestMergeExistingConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.Setenv("EMBED_PROXY_CONFIG", cfgPath); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv("EMBED_PROXY_CONFIG")

	existing := `{"port":17000,"siliconflow_api_key":"sk-old-key-1234","upstream_base":"http://mock/v1","proxy_auth_key":"auth-keep","route_mappings":[{"source":"/a","target":"/b"}]}`
	if err := os.WriteFile(cfgPath, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := defaultConfig()
	merged := mergeExistingConfig(cfg)

	if merged.Port != 17000 {
		t.Errorf("端口应保留 17000，得到 %d", merged.Port)
	}
	if merged.SiliconflowAPIKey != "sk-old-key-1234" {
		t.Errorf("Key 应保留，得到 %s", merged.SiliconflowAPIKey)
	}
	if merged.UpstreamBase != "http://mock/v1" {
		t.Errorf("上游应保留，得到 %s", merged.UpstreamBase)
	}
	if merged.ProxyAuthKey != "auth-keep" {
		t.Errorf("鉴权 Key 应保留，得到 %s", merged.ProxyAuthKey)
	}
	if len(merged.RouteMappings) != 1 || merged.RouteMappings[0].Source != "/a" {
		t.Errorf("路由映射应保留，得到 %+v", merged.RouteMappings)
	}
}

func TestMergeExistingConfigNoFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "nope.json")
	if err := os.Setenv("EMBED_PROXY_CONFIG", cfgPath); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv("EMBED_PROXY_CONFIG")

	cfg := defaultConfig()
	cfg.SiliconflowAPIKey = "sk-new"
	merged := mergeExistingConfig(cfg)

	if merged.Port != defaultPort {
		t.Errorf("无配置文件时端口应为 %d，得到 %d", defaultPort, merged.Port)
	}
	if merged.ProxyAuthKey == "" {
		t.Error("无配置文件时应自动生成随机鉴权 Key")
	}
}

// ---------- 代理 handler ----------

func newTestHandler(cfg Config) http.Handler {
	if cfg.ProxyAuthKey == "" {
		cfg.ProxyAuthKey = "test-auth-key"
	}
	return newProxyHandler(&cfg)
}

func doRequest(t *testing.T, h http.Handler, method, path, authKey, body string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, reader)
	if authKey != "" {
		req.Header.Set("Authorization", "Bearer "+authKey)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result(), rec.Body.Bytes()
}

// 测试 1：鉴权 —— 缺 Key / 错 Key 必须 401
func TestAuthRequired(t *testing.T) {
	cfg := Config{Port: 16540, UpstreamBase: "http://mock/v1", ProxyAuthKey: "secret123"}
	h := newTestHandler(cfg)

	for name, auth := range map[string]string{
		"无 Authorization 头": "",
		"错误 Key":            "wrong",
	} {
		resp, body := doRequest(t, h, "POST", "/v1/embeddings", auth, `{"model":"m","input":["x"]}`)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: 应返回 401，得到 %d (%s)", name, resp.StatusCode, body)
		}
	}

	// 正确 Key：应通过鉴权（上游不可达返回 502，而非 401）
	resp, _ := doRequest(t, h, "POST", "/v1/embeddings", "secret123", `{"model":"m","input":["x"]}`)
	if resp.StatusCode == http.StatusUnauthorized {
		t.Error("正确 Key 不应 401")
	}
}

// 测试 2：/v1/embeddings 删除 encoding_format 后再转发
func TestStripEncodingFormatOnForward(t *testing.T) {
	var received map[string]interface{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &received)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := Config{Port: 16540, UpstreamBase: upstream.URL + "/v1", ProxyAuthKey: "secret123"}
	h := newTestHandler(cfg)

	_, body := doRequest(t, h, "POST", "/v1/embeddings", "secret123",
		`{"model":"BAAI/bge-m3","input":["测试"],"encoding_format":null}`)
	if string(body) != `{"ok":true}` {
		t.Fatalf("上游响应未透传: %s", body)
	}
	if received == nil {
		t.Fatal("上游未收到请求")
	}
	if _, ok := received["encoding_format"]; ok {
		t.Error("上游收到的请求仍包含 encoding_format 字段！")
	}
	if received["model"] != "BAAI/bge-m3" {
		t.Errorf("model 应透传，得到 %v", received["model"])
	}
}

// 测试 3：自定义路由映射 /v1/audio/transcriptions -> /v1/chat/completions
func TestRouteMapping(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		data, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	}))
	defer upstream.Close()

	cfg := Config{
		Port: 16540, UpstreamBase: upstream.URL + "/v1", ProxyAuthKey: "secret123",
		RouteMappings: []RouteMapping{{Source: "/v1/audio/transcriptions", Target: "/v1/chat/completions"}},
	}
	h := newTestHandler(cfg)

	body := `{"file":"audio.mp3","model":"whisper","encoding_format":null}`
	resp, respBody := doRequest(t, h, "POST", "/v1/audio/transcriptions", "secret123", body)
	if resp.StatusCode != 200 {
		t.Fatalf("应 200，得到 %d", resp.StatusCode)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("路由应改写为 /v1/chat/completions，实际 %s", gotPath)
	}
	// 非 embedding 路径：body 原样透传，不做 encoding_format 清洗
	var echoed map[string]interface{}
	if err := json.Unmarshal(respBody, &echoed); err != nil {
		t.Fatalf("body 透传失败: %s", respBody)
	}
	if _, ok := echoed["encoding_format"]; !ok {
		t.Error("非 embedding 路径不应删除 encoding_format")
	}
}

// 测试 4：自定义上游地址生效，Authorization 替换为上游 Key
func TestCustomUpstream(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := Config{Port: 16540, UpstreamBase: upstream.URL + "/custom/v1", ProxyAuthKey: "secret123", SiliconflowAPIKey: "sk-upstream-real"}
	h := newTestHandler(cfg)

	doRequest(t, h, "POST", "/v1/embeddings", "secret123", `{"model":"m","input":["x"]}`)
	if gotAuth != "Bearer sk-upstream-real" {
		t.Errorf("上游 Authorization 应替换为上游 Key，得到 %s", gotAuth)
	}
}
