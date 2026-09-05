package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 测试：已有配置文件时，安装/重装服务不得覆盖用户已保存的 Key/端口/上游
func TestMergeExistingConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.Setenv("EMBED_PROXY_CONFIG", cfgPath); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv("EMBED_PROXY_CONFIG")

	// 已有配置：用户保存过 Key 和自定义端口
	existing := `{"port":17000,"siliconflow_api_key":"sk-old-key-1234","upstream_base":"https://api.siliconflow.cn/v1"}`
	if err := os.WriteFile(cfgPath, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	// 模拟新进程内存里的默认配置（没设 Key、默认端口）
	cfg := defaultConfig()
	merged := mergeExistingConfig(cfg)

	if merged.Port != 17000 {
		t.Errorf("端口应保留 17000，得到 %d", merged.Port)
	}
	if merged.SiliconflowAPIKey != "sk-old-key-1234" {
		t.Errorf("Key 应保留 sk-old-key-1234，得到 %s", merged.SiliconflowAPIKey)
	}
	if merged.UpstreamBase != "https://api.siliconflow.cn/v1" {
		t.Errorf("上游应保留，得到 %s", merged.UpstreamBase)
	}
}

// 测试：没有配置文件时，用当前配置（默认端口 16540）
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
	if merged.SiliconflowAPIKey != "sk-new" {
		t.Errorf("无配置文件时应使用当前 Key，得到 %s", merged.SiliconflowAPIKey)
	}
}
