// embed-proxy v1.2.0 (multi-instance)
// SiliconFlow Embedding 清洗代理
// 核心功能：
//  1. 接收 /v1/embeddings，删除 encoding_format 字段后转发上游，解决 LiteLLM/OpenWebUI
//     注入 "encoding_format":null 导致硅基返回 20015 参数非法的问题
//  2. 全局 API Key 鉴权：请求必须携带 Authorization: Bearer <proxy_auth_key>，防止公网滥用
//  3. 自定义路由映射：/v1/audio/transcriptions -> /v1/chat/completions 等任意路径改写
//  4. 自定义上游 API 地址（默认硅基流动 https://api.siliconflow.cn/v1）
//
// 附带功能：内网穿透友好（默认监听 0.0.0.0）；交互菜单一键安装 systemd 服务（开机自启）、卸载、
//
//	改端口、设 Key、改上游、管理路由映射；重装服务自动保留已有配置。
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	version         = "1.2.1"
	defaultPort     = 16540
	defaultUpstream = "https://api.siliconflow.cn/v1"
	configDir       = "/etc/embed-proxy"
	configPath      = "/etc/embed-proxy/config.json"
	binPath         = "/usr/local/bin/embed-proxy"
	unitPath        = "/etc/systemd/system/embed-proxy.service"
	serviceName     = "embed-proxy.service"
	envKeyVar       = "SILICONFLOW_API_KEY"
	envPortVar      = "EMBED_PROXY_PORT"
	envInstanceVar  = "EMBED_PROXY_INSTANCE"
	requestTimeout  = 120 * time.Second
	embeddingPath   = "/v1/embeddings"
	maxBodySize     = 32 << 20
)

// RouteMapping 自定义路由映射：Source 命中后转发到 Target
type RouteMapping struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// Config 代理配置
type Config struct {
	Port              int            `json:"port"`
	SiliconflowAPIKey string         `json:"siliconflow_api_key"` // 上游 API Key（硅基）
	UpstreamBase      string         `json:"upstream_base"`       // 自定义上游地址，默认硅基
	ProxyAuthKey      string         `json:"proxy_auth_key"`      // 代理访问鉴权 Key（防公网滥用）
	RouteMappings     []RouteMapping `json:"route_mappings"`      // 自定义路由映射
}

func randomKey(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("k%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func defaultConfig() Config {
	return Config{
		Port:         defaultPort,
		UpstreamBase: defaultUpstream,
		ProxyAuthKey: randomKey(24), // 首次运行自动生成随机鉴权 Key
		RouteMappings: []RouteMapping{
			{Source: "/v1/audio/transcriptions", Target: "/v1/chat/completions"},
		},
	}
}

// currentInstance 当前实例名，default 为经典单实例（兼容旧部署路径）
var currentInstance = "default"

// parseInstance 从环境变量或 --instance 参数解析实例名
func parseInstance() string {
	if v := os.Getenv(envInstanceVar); v != "" {
		return v
	}
	for i, a := range os.Args {
		if a == "--instance" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return "default"
}

// configPathEnv 当前实例的配置路径（EMBED_PROXY_CONFIG 显式指定时优先）
func configPathEnv() string {
	if v := os.Getenv("EMBED_PROXY_CONFIG"); v != "" {
		return v
	}
	if currentInstance == "" || currentInstance == "default" {
		return configPath
	}
	return filepath.Join(configDir, currentInstance+".json")
}

func configPathFor(instance string) string {
	if instance == "" || instance == "default" {
		return configPath
	}
	return filepath.Join(configDir, instance+".json")
}

// binPathFor 每个实例使用独立命名的二进制，default 兼容旧路径 /usr/local/bin/embed-proxy
func binPathFor(instance string) string {
	if instance == "" || instance == "default" {
		return binPath
	}
	return "/usr/local/bin/embed-proxy-" + instance
}

func unitNameFor(instance string) string {
	if instance == "" || instance == "default" {
		return serviceName
	}
	return "embed-proxy-" + instance + ".service"
}

func unitPathFor(instance string) string {
	return filepath.Join("/etc/systemd/system", unitNameFor(instance))
}

// listInstances 从 systemd unit 与配置文件发现所有实例
func listInstances() []string {
	seen := map[string]bool{}
	matches, _ := filepath.Glob("/etc/systemd/system/embed-proxy*.service")
	for _, m := range matches {
		name := strings.TrimSuffix(filepath.Base(m), ".service")
		if strings.HasPrefix(name, "embed-proxy-") {
			seen[strings.TrimPrefix(name, "embed-proxy-")] = true
		} else {
			seen["default"] = true
		}
	}
	if entries, err := os.ReadDir(configDir); err == nil {
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			if e.Name() == "config.json" {
				seen["default"] = true
			} else {
				seen[strings.TrimSuffix(e.Name(), ".json")] = true
			}
		}
	}
	seen["default"] = true
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// 首次运行：生成默认配置（含随机鉴权 Key）并落盘，保证 Key 可查
		if err := saveConfig(path, cfg); err != nil {
			return cfg, err
		}
		logf("[config] 已生成默认配置 %s，请用菜单查看/修改 代理鉴权 Key", path)
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		cfg.Port = defaultPort
	}
	if cfg.UpstreamBase == "" {
		cfg.UpstreamBase = defaultUpstream
	}
	if cfg.RouteMappings == nil {
		cfg.RouteMappings = defaultConfig().RouteMappings
	}
	// 环境变量优先
	if v := os.Getenv(envKeyVar); v != "" {
		cfg.SiliconflowAPIKey = v
	}
	if v := os.Getenv(envPortVar); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p <= 65535 {
			cfg.Port = p
		}
	}
	return cfg, nil
}

func saveConfig(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// ---------- HTTP 代理 ----------

// resolveRoute 根据自定义路由映射改写路径，未命中原样返回
func resolveRoute(path string, mappings []RouteMapping) string {
	for _, m := range mappings {
		if m.Source == path {
			return m.Target
		}
	}
	return path
}

// buildUpstreamURL 拼接上游地址与目标路径，自动去重 /v1 前缀
// 例: base=https://api.siliconflow.cn/v1, target=/v1/chat/completions
//
//	-> https://api.siliconflow.cn/v1/chat/completions
func buildUpstreamURL(base, target string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(target, "/v1") {
		target = strings.TrimPrefix(target, "/v1")
		if target == "" {
			target = "/"
		}
	}
	return base + target
}

func newProxyHandler(cfg *Config) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","port":%d,"version":"%s"}`, cfg.Port, version)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 1. 全局鉴权：ProxyAuthKey 非空时校验 Bearer，防止公网被滥用
		if cfg.ProxyAuthKey != "" && r.Header.Get("Authorization") != "Bearer "+cfg.ProxyAuthKey {
			writeError(w, http.StatusUnauthorized, "invalid or missing API key")
			return
		}

		// 2. 自定义路由映射
		targetPath := resolveRoute(r.URL.Path, cfg.RouteMappings)
		upstreamURL := buildUpstreamURL(cfg.UpstreamBase, targetPath)

		// 3. 读 body
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
		if err != nil {
			writeError(w, http.StatusBadRequest, "read body failed: "+err.Error())
			return
		}

		// 4. embedding 请求：删除 encoding_format 字段（LiteLLM 注入 null 的根源）
		if r.URL.Path == embeddingPath && len(body) > 0 {
			cleaned, err := stripEncodingFormat(body)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
				return
			}
			body = cleaned
		}

		// 5. 转发上游（超时保护）
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, r.Method, upstreamURL, bytes.NewReader(body))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "build upstream request failed")
			return
		}
		// 透传请求头（Authorization 替换为上游 Key）
		for k, v := range r.Header {
			if !strings.EqualFold(k, "Authorization") && !strings.EqualFold(k, "Content-Length") {
				req.Header[k] = v
			}
		}
		req.Header.Set("Authorization", "Bearer "+cfg.SiliconflowAPIKey)
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: requestTimeout}
		resp, err := client.Do(req)
		if err != nil {
			writeError(w, http.StatusBadGateway, "upstream error: "+err.Error())
			return
		}
		defer resp.Body.Close()
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			writeError(w, http.StatusBadGateway, "read upstream response failed")
			return
		}
		// 透传上游响应头
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		logf("[proxy] %s %s -> %s -> %d (%d bytes)", r.Method, r.URL.Path, targetPath, resp.StatusCode, len(respBody))
	})

	return logMiddleware(mux)
}

// stripEncodingFormat 解析 JSON，删除 encoding_format 键，重新序列化
func stripEncodingFormat(body []byte) ([]byte, error) {
	var m map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	delete(m, "encoding_format")
	return json.Marshal(m)
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/healthz" {
			logf("[http] %s %s %s (%s)", r.Method, r.URL.Path, r.RemoteAddr, time.Since(start).Round(time.Millisecond))
		}
	})
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]interface{}{"message": msg, "type": "embed_proxy_error"}})
}

func runServer(cfg Config) error {
	addr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("端口 %d 被占用或无法监听: %w", cfg.Port, err)
	}
	defer ln.Close()

	if cfg.SiliconflowAPIKey == "" {
		logf("[warn] 尚未设置上游 API Key，请求将返回 401（请用菜单设置，或设置环境变量 %s）", envKeyVar)
	}
	if cfg.ProxyAuthKey == "" {
		logf("[warn] 代理鉴权 Key 为空，任何请求均可访问！公网暴露时请务必设置")
	} else {
		logf("[config] 代理鉴权 Key: %s（客户端请求需携带 Authorization: Bearer %s）", cfg.ProxyAuthKey, cfg.ProxyAuthKey)
	}

	srv := &http.Server{Handler: newProxyHandler(&cfg), ReadHeaderTimeout: 10 * time.Second}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		logf("收到退出信号，正在关闭…")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	logf("embed-proxy v%s 已启动，监听 %s （0.0.0.0，可被内网穿透工具转发）", version, addr)
	logf("上游: %s  |  健康检查: http://<host>:%d/healthz", cfg.UpstreamBase, cfg.Port)
	return srv.Serve(ln)
}

// ---------- systemd 服务管理 ----------

func isRoot() bool { return os.Geteuid() == 0 }

func hasSystemd() bool {
	_, err := os.Stat("/run/systemd/system")
	return err == nil
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// mergeExistingConfig 合并已存在的配置：已有配置中的 Key/端口/上游/鉴权/路由优先，
// 确保重装/升级系统服务时用户已保存的数据不被覆盖丢失。
func mergeExistingConfig(cfg Config) Config {
	data, err := os.ReadFile(configPathEnv())
	if err != nil {
		return cfg
	}
	var existing Config
	if err := json.Unmarshal(data, &existing); err != nil {
		return cfg
	}
	if existing.SiliconflowAPIKey != "" {
		cfg.SiliconflowAPIKey = existing.SiliconflowAPIKey
	}
	if existing.Port > 0 && existing.Port <= 65535 {
		cfg.Port = existing.Port
	}
	if existing.UpstreamBase != "" {
		cfg.UpstreamBase = existing.UpstreamBase
	}
	if existing.ProxyAuthKey != "" {
		cfg.ProxyAuthKey = existing.ProxyAuthKey
	}
	if len(existing.RouteMappings) > 0 {
		cfg.RouteMappings = existing.RouteMappings
	}
	logf("[config] 检测到已有配置，已合并保留 (端口=%d, 上游=%s)", cfg.Port, cfg.UpstreamBase)
	return cfg
}

// installService 把自身复制到 /usr/local/bin，写入配置与 unit，enable --now
func installService(cfg Config) error {
	if !isRoot() {
		return errors.New("安装系统服务需要 root 权限，请用 sudo 运行")
	}
	if !hasSystemd() {
		return errors.New("未检测到 systemd（/run/systemd/system 不存在），无法安装系统服务")
	}
	targetBin := binPathFor(currentInstance)
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self != targetBin {
		if _, err := os.Stat(targetBin); err == nil {
			logf("[install] %s 已存在，跳过复制（该实例使用独立二进制）", targetBin)
		} else {
			if err := copyFile(self, targetBin); err != nil {
				return fmt.Errorf("复制二进制到 %s 失败: %w", targetBin, err)
			}
			if err := os.Chmod(targetBin, 0o755); err != nil {
				return err
			}
			logf("[install] 二进制已复制到 %s", targetBin)
		}
	}
	// 合并保留已有配置，再落盘
	cfg = mergeExistingConfig(cfg)
	if err := saveConfig(configPathEnv(), cfg); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	unit := fmt.Sprintf(`[Unit]
Description=SiliconFlow Embedding Proxy (embed-proxy, instance=%s)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s --daemon --config %s --instance %s
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`, currentInstance, binPathFor(currentInstance), configPathEnv(), currentInstance)
	if err := os.WriteFile(unitPathFor(currentInstance), []byte(unit), 0o644); err != nil {
		return fmt.Errorf("写入 systemd unit 失败: %w", err)
	}
	_ = systemctl("daemon-reload")
	if err := systemctl("enable", "--now", unitNameFor(currentInstance)); err != nil {
		return fmt.Errorf("systemctl enable --now 失败: %w", err)
	}
	logf("✅ 已安装并启动系统服务 %s（开机自启已启用）", unitNameFor(currentInstance))
	logf("   监听端口: %d   健康检查: http://<host>:%d/healthz", cfg.Port, cfg.Port)
	return nil
}

func maskKey(key string) string {
	if key == "" {
		return "未设置"
	}
	if len(key) > 8 {
		return key[:4] + "****" + key[len(key)-4:]
	}
	return "****"
}

func uninstallService(keepConfig bool) error {
	if !isRoot() {
		return errors.New("卸载系统服务需要 root 权限，请用 sudo 运行")
	}
	if !hasSystemd() {
		return errors.New("未检测到 systemd")
	}
	_ = systemctl("disable", "--now", unitNameFor(currentInstance))
	_ = os.Remove(unitPathFor(currentInstance))
	_ = systemctl("daemon-reload")
	if currentInstance != "default" {
		if err := os.Remove(binPathFor(currentInstance)); err == nil {
			logf("[uninstall] 已删除实例二进制 %s", binPathFor(currentInstance))
		}
	}
	if !keepConfig {
		cfgPath := configPathFor(currentInstance)
		_ = os.Remove(cfgPath)
		if currentInstance == "default" {
			_ = os.RemoveAll(configDir)
		}
	}
	if keepConfig {
		logf("✅ 已卸载系统服务（实例 %s 配置保留在 %s）", currentInstance, configPathFor(currentInstance))
	} else {
		logf("✅ 已卸载系统服务（实例 %s 配置已删除）", currentInstance)
	}
	return nil
}

func serviceStatus() string {
	unit := unitNameFor(currentInstance)
	cmd := exec.Command("systemctl", "is-active", unit)
	out, _ := cmd.Output()
	state := strings.TrimSpace(string(out))
	if state == "" {
		state = "未安装/未知"
	}
	enabled := "否"
	cmd2 := exec.Command("systemctl", "is-enabled", unit)
	out2, _ := cmd2.Output()
	if strings.TrimSpace(string(out2)) == "enabled" {
		enabled = "是"
	}
	return fmt.Sprintf("服务状态: %s | 开机自启: %s | unit: %s", state, enabled, unit)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// ---------- 交互菜单 ----------

func printBanner(cfg Config) {
	status := "未安装"
	if hasSystemd() {
		cmd := exec.Command("systemctl", "is-active", serviceName)
		if out, err := cmd.Output(); err == nil && strings.TrimSpace(string(out)) == "active" {
			status = "运行中"
		}
	}
	fmt.Println("==================================================")
	fmt.Printf("  Embedding 清洗代理 v%s（硅基流动适配）\n", version)
	fmt.Println("  功能: encoding_format 清洗 / 全局鉴权 / 自定义路由 / 自定义上游")
	fmt.Println("--------------------------------------------------")
	fmt.Printf("  当前实例 : %s\n", currentInstance)
	fmt.Printf("  当前端口 : %d (默认 %d)\n", cfg.Port, defaultPort)
	fmt.Printf("  上游地址 : %s\n", cfg.UpstreamBase)
	fmt.Printf("  上游 Key : %s\n", maskKey(cfg.SiliconflowAPIKey))
	fmt.Printf("  鉴权 Key : %s\n", maskKey(cfg.ProxyAuthKey))
	fmt.Printf("  服务状态 : %s\n", status)
	fmt.Println("--------------------------------------------------")
	fmt.Println("  1) 安装为系统服务（开机自启动）")
	fmt.Println("  2) 卸载系统服务")
	fmt.Println("  3) 修改监听端口")
	fmt.Println("  4) 设置上游 API Key（硅基 sk-xxx）")
	fmt.Println("  5) 设置代理鉴权 Key（公网防滥用）")
	fmt.Println("  6) 修改上游 API 地址（默认硅基流动）")
	fmt.Println("  7) 管理自定义路由映射")
	fmt.Println("  8) 切换/管理实例（多实例）")
	fmt.Println("  9) 前台启动代理（当前终端, Ctrl+C 停止）")
	fmt.Println(" 10) 查看服务状态")
	fmt.Println("  0) 退出")
	fmt.Println("==================================================")
}

func readLine(reader *bufio.Reader) string {
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line)
}

func askYesNo(reader *bufio.Reader, prompt string) bool {
	fmt.Printf("%s [y/N]: ", prompt)
	ans := strings.ToLower(readLine(reader))
	return ans == "y" || ans == "yes"
}

func manageRoutes(reader *bufio.Reader, cfg *Config) {
	for {
		fmt.Println("--------------------------------------------------")
		fmt.Println("  当前路由映射规则:")
		if len(cfg.RouteMappings) == 0 {
			fmt.Println("    （无，所有路径原样转发）")
		}
		for i, r := range cfg.RouteMappings {
			fmt.Printf("    %d. %s  ->  %s\n", i+1, r.Source, r.Target)
		}
		fmt.Println("--------------------------------------------------")
		fmt.Println("  a) 添加规则    d) 删除规则   0) 返回")
		fmt.Print("  请选择: ")
		opt := readLine(reader)
		switch opt {
		case "a":
			fmt.Print("  源路径（如 /v1/audio/transcriptions）: ")
			src := readLine(reader)
			fmt.Print("  目标路径（如 /v1/chat/completions）: ")
			tgt := readLine(reader)
			if src == "" || tgt == "" || !strings.HasPrefix(src, "/") || !strings.HasPrefix(tgt, "/") {
				fmt.Println("  ❌ 路径必须以 / 开头且不能为空")
				continue
			}
			// 覆盖已有同名源路径
			found := false
			for i := range cfg.RouteMappings {
				if cfg.RouteMappings[i].Source == src {
					cfg.RouteMappings[i].Target = tgt
					found = true
					break
				}
			}
			if !found {
				cfg.RouteMappings = append(cfg.RouteMappings, RouteMapping{Source: src, Target: tgt})
			}
			if err := saveConfig(configPathEnv(), *cfg); err != nil {
				fmt.Printf("  ❌ 保存失败: %v\n", err)
			} else {
				fmt.Println("  ✅ 路由规则已保存")
			}
		case "d":
			fmt.Print("  输入要删除的规则序号: ")
			idxStr := readLine(reader)
			idx, err := strconv.Atoi(idxStr)
			if err != nil || idx < 1 || idx > len(cfg.RouteMappings) {
				fmt.Println("  ❌ 序号不合法")
				continue
			}
			cfg.RouteMappings = append(cfg.RouteMappings[:idx-1], cfg.RouteMappings[idx:]...)
			if err := saveConfig(configPathEnv(), *cfg); err != nil {
				fmt.Printf("  ❌ 保存失败: %v\n", err)
			} else {
				fmt.Println("  ✅ 已删除")
			}
		case "0":
			return
		default:
			fmt.Println("  ❌ 无效选项")
		}
	}
}

// manageInstances 列出所有实例并切换当前实例（可新建）
func manageInstances(reader *bufio.Reader, cfg *Config) {
	for {
		instances := listInstances()
		fmt.Println("--------------------------------------------------")
		fmt.Println("  已发现实例:")
		for i, name := range instances {
			mark := " "
			if name == currentInstance {
				mark = "*"
			}
			st := ""
			if hasSystemd() {
				cmd := exec.Command("systemctl", "is-active", unitNameFor(name))
				if out, err := cmd.Output(); err == nil {
					st = " [" + strings.TrimSpace(string(out)) + "]"
				}
			}
			// 尝试读端口显示
			portStr := ""
			if data, err := os.ReadFile(configPathFor(name)); err == nil {
				var c Config
				if json.Unmarshal(data, &c) == nil && c.Port > 0 {
					portStr = fmt.Sprintf(" :%d", c.Port)
				}
			}
			fmt.Printf("   %s %d. %s%s%s\n", mark, i+1, name, portStr, st)
		}
		fmt.Println("--------------------------------------------------")
		fmt.Println("  输入编号切换实例；输入新名字创建实例；0 返回")
		fmt.Print("  请选择: ")
		opt := readLine(reader)
		if opt == "0" {
			return
		}
		if idx, err := strconv.Atoi(opt); err == nil && idx >= 1 && idx <= len(instances) {
			currentInstance = instances[idx-1]
			nc, err := loadConfig(configPathFor(currentInstance))
			if err != nil {
				nc = defaultConfig()
			}
			*cfg = nc
			fmt.Printf("✅ 已切换到实例: %s（配置 %s）\n", currentInstance, configPathFor(currentInstance))
			return
		}
		if opt != "" {
			// 新实例名：仅允许字母数字与下划线
			if !regexpMustValid(opt) {
				fmt.Println("  ❌ 实例名只能包含字母/数字/下划线/中划线")
				continue
			}
			currentInstance = opt
			nc, err := loadConfig(configPathFor(currentInstance))
			if err != nil {
				nc = defaultConfig()
			}
			*cfg = nc
			fmt.Printf("✅ 已切换到实例: %s（新实例，请用菜单配置并安装服务）\n", currentInstance)
			return
		}
		fmt.Println("  ❌ 无效选择")
	}
}

func regexpMustValid(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func interactive() {
	cfg, err := loadConfig(configPathEnv())
	if err != nil {
		fmt.Printf("加载配置失败: %v，使用默认配置\n", err)
		cfg = defaultConfig()
	}
	reader := bufio.NewReader(os.Stdin)

	for {
		printBanner(cfg)
		fmt.Print("请选择: ")
		choice := readLine(reader)
		switch choice {
		case "1":
			if !isRoot() {
				fmt.Println("❌ 需要 root 权限，请用 sudo 运行本程序")
				break
			}
			if cfg.SiliconflowAPIKey == "" {
				fmt.Print("当前未设置上游 API Key，请输入（形如 sk-xxxx）: ")
				key := readLine(reader)
				if key == "" {
					fmt.Println("❌ 未输入 Key，取消安装")
					break
				}
				cfg.SiliconflowAPIKey = key
			}
			if err := installService(cfg); err != nil {
				fmt.Printf("❌ 安装失败: %v\n", err)
			}
		case "2":
			if !isRoot() {
				fmt.Println("❌ 需要 root 权限，请用 sudo 运行本程序")
				break
			}
			del := askYesNo(reader, "是否【删除】配置文件（Key/端口/路由将丢失）？选 n 保留配置")
			if err := uninstallService(!del); err != nil {
				fmt.Printf("❌ 卸载失败: %v\n", err)
			}
		case "3":
			fmt.Printf("当前端口: %d，请输入新端口 (1-65535): ", cfg.Port)
			input := readLine(reader)
			p, err := strconv.Atoi(input)
			if err != nil || p < 1 || p > 65535 {
				fmt.Println("❌ 端口不合法，未修改")
				break
			}
			cfg.Port = p
			if err := saveConfig(configPathEnv(), cfg); err != nil {
				fmt.Printf("❌ 保存配置失败: %v\n", err)
				break
			}
			fmt.Printf("✅ 端口已改为 %d\n", p)
			if hasSystemd() {
				cmd := exec.Command("systemctl", "is-active", serviceName)
				if out, err := cmd.Output(); err == nil && strings.TrimSpace(string(out)) == "active" {
					fmt.Println("   服务正在运行，执行 systemctl restart embed-proxy 生效…")
					_ = systemctl("restart", serviceName)
					fmt.Println("   ✅ 服务已重启，新端口生效")
				}
			}
		case "4":
			fmt.Print("请输入上游 API Key（硅基 sk-xxxx）: ")
			key := readLine(reader)
			if key == "" {
				fmt.Println("❌ Key 不能为空")
				break
			}
			cfg.SiliconflowAPIKey = key
			if err := saveConfig(configPathEnv(), cfg); err != nil {
				fmt.Printf("❌ 保存失败: %v\n", err)
			} else {
				fmt.Println("✅ 上游 API Key 已保存")
			}
		case "5":
			fmt.Printf("当前代理鉴权 Key: %s\n", cfg.ProxyAuthKey)
			fmt.Print("输入新鉴权 Key（留空则随机生成）: ")
			key := readLine(reader)
			if key == "" {
				key = randomKey(24)
			}
			cfg.ProxyAuthKey = key
			if err := saveConfig(configPathEnv(), cfg); err != nil {
				fmt.Printf("❌ 保存失败: %v\n", err)
			} else {
				fmt.Printf("✅ 代理鉴权 Key 已保存: %s\n", key)
				fmt.Println("   客户端请求需携带: Authorization: Bearer " + key)
			}
		case "6":
			fmt.Printf("当前上游地址: %s\n", cfg.UpstreamBase)
			fmt.Print("输入新上游地址（如 https://dashscope.aliyuncs.com/compatible-mode/v1，回车恢复默认硅基）: ")
			input := readLine(reader)
			if input == "" {
				cfg.UpstreamBase = defaultUpstream
			} else {
				if !strings.HasPrefix(input, "http://") && !strings.HasPrefix(input, "https://") {
					fmt.Println("❌ 地址必须以 http:// 或 https:// 开头")
					break
				}
				cfg.UpstreamBase = strings.TrimRight(input, "/")
			}
			if err := saveConfig(configPathEnv(), cfg); err != nil {
				fmt.Printf("❌ 保存失败: %v\n", err)
			} else {
				fmt.Printf("✅ 上游地址已保存: %s\n", cfg.UpstreamBase)
			}
		case "7":
			manageRoutes(reader, &cfg)
		case "8":
			manageInstances(reader, &cfg)
		case "9":
			fmt.Println("前台启动中…（Ctrl+C 停止）")
			if err := runServer(cfg); err != nil {
				fmt.Printf("❌ 启动失败: %v\n", err)
			}
		case "10":
			fmt.Println(serviceStatus())
		case "0", "q", "Q":
			fmt.Println("再见 👋")
			return
		default:
			fmt.Println("❌ 无效选项")
		}
		fmt.Println()
	}
}

// ---------- 入口 ----------

func main() {
	logf("embed-proxy v%s starting (args: %s)", version, strings.Join(os.Args[1:], " "))
	for _, a := range os.Args[1:] {
		if a == "--help" || a == "-h" {
			fmt.Println("embed-proxy v" + version)
			fmt.Println("用法: embed-proxy [--instance <name>] [--daemon] [--config <path>]")
			fmt.Println("  --instance <name>  指定实例名（默认 default；命名实例独立配置/独立二进制/独立服务）")
			fmt.Println("  --daemon           后台服务模式（由 systemd 调用）")
			fmt.Println("  --config <path>    指定配置文件路径（默认按实例自动选择）")
			fmt.Println("  不带参数启动进入交互菜单")
			return
		}
	}
	currentInstance = parseInstance()
	// --daemon: 服务模式，不起菜单
	if len(os.Args) > 1 && os.Args[1] == "--daemon" {
		cfgPath := configPathEnv()
		for i, a := range os.Args {
			if a == "--config" && i+1 < len(os.Args) {
				cfgPath = os.Args[i+1]
			}
		}
		cfg, err := loadConfig(cfgPath)
		if err != nil {
			logf("[fatal] 加载配置失败 %s: %v", cfgPath, err)
			os.Exit(1)
		}
		if err := runServer(cfg); err != nil {
			logf("[fatal] %v", err)
			os.Exit(1)
		}
		return
	}
	// 交互模式
	interactive()
}

func logf(format string, args ...interface{}) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	fmt.Printf("[%s] %s\n", ts, fmt.Sprintf(format, args...))
}
