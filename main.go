// embed-proxy v1.0.0
// SiliconFlow Embedding 清洗代理
// 核心功能：接收 /v1/embeddings 请求，删除 encoding_format 字段后转发给硅基流动，
//          解决 LiteLLM/OpenWebUI 注入 "encoding_format":null 导致硅基返回 20015 参数非法的问题。
// 附带功能：内网穿透友好（默认监听 0.0.0.0，可被 cloudflared/frp 等隧道直接转发）；
//          交互菜单支持一键安装 systemd 服务（开机自启）、卸载、改端口。
package main

import (
	"bufio"
	"bytes"
	"context"
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
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	version          = "1.0.0"
	defaultPort      = 16540
	defaultUpstream  = "https://api.siliconflow.cn/v1"
	configDir        = "/etc/embed-proxy"
	configPath       = "/etc/embed-proxy/config.json"
	binPath          = "/usr/local/bin/embed-proxy"
	unitPath         = "/etc/systemd/system/embed-proxy.service"
	serviceName      = "embed-proxy.service"
	envKeyVar        = "SILICONFLOW_API_KEY"
	envPortVar       = "EMBED_PROXY_PORT"
	requestTimeout   = 120 * time.Second
	upstreamEndpoint = "/embeddings"
)

// Config 代理配置
type Config struct {
	Port              int    `json:"port"`
	SiliconflowAPIKey string `json:"siliconflow_api_key"`
	UpstreamBase      string `json:"upstream_base"`
}

func defaultConfig() Config {
	return Config{
		Port:         defaultPort,
		UpstreamBase: defaultUpstream,
	}
}

// envOr 环境变量取配置路径（默认 /etc/embed-proxy/config.json）
func configPathEnv() string {
	if v := os.Getenv("EMBED_PROXY_CONFIG"); v != "" {
		return v
	}
	return configPath
}

func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
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

func newProxyHandler(cfg *Config) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","port":%d,"version":"%s"}`, cfg.Port, version)
	})

	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "only POST allowed")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		if err != nil {
			writeError(w, http.StatusBadRequest, "read body failed: "+err.Error())
			return
		}
		// 核心：删除 encoding_format 字段（LiteLLM 会注入 null）
		cleaned, err := stripEncodingFormat(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
			return
		}
		upstreamURL := strings.TrimRight(cfg.UpstreamBase, "/") + upstreamEndpoint
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(cleaned))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "build upstream request failed")
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cfg.SiliconflowAPIKey)
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		logf("[proxy] %s %s -> %d (%d bytes)", r.Method, r.URL.Path, resp.StatusCode, len(respBody))
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
	// 预先探测端口占用
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("端口 %d 被占用或无法监听: %w", cfg.Port, err)
	}
	defer ln.Close()

	if cfg.SiliconflowAPIKey == "" {
		logf("[warn] 尚未设置硅基 API Key，embedding 请求将返回 401（请运行程序进入菜单设置，或设置环境变量 %s）", envKeyVar)
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
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return false
	}
	return true
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// mergeExistingConfig 合并已存在的配置：已有配置中的 Key/端口/上游优先，
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
	logf("[config] 检测到已有配置，已合并保留 (端口=%d, Key=%s)", cfg.Port, maskKey(cfg.SiliconflowAPIKey))
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
	// 复制自身
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self != binPath {
		if err := copyFile(self, binPath); err != nil {
			return fmt.Errorf("复制二进制到 %s 失败: %w", binPath, err)
		}
		if err := os.Chmod(binPath, 0o755); err != nil {
			return err
		}
	}
	// 合并保留已有配置（Key/端口/上游），再落盘
	cfg = mergeExistingConfig(cfg)
	if err := saveConfig(configPathEnv(), cfg); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	// 写 unit
	unit := fmt.Sprintf(`[Unit]
Description=SiliconFlow Embedding Proxy (embed-proxy)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s --daemon --config %s
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`, binPath, configPathEnv())
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("写入 systemd unit 失败: %w", err)
	}
	_ = systemctl("daemon-reload")
	if err := systemctl("enable", "--now", serviceName); err != nil {
		return fmt.Errorf("systemctl enable --now 失败: %w", err)
	}
	logf("✅ 已安装并启动系统服务 %s（开机自启已启用）", serviceName)
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
	_ = systemctl("disable", "--now", serviceName)
	_ = os.Remove(unitPath)
	_ = systemctl("daemon-reload")
	if !keepConfig {
		_ = os.RemoveAll(configDir)
	}
	if keepConfig {
		logf("✅ 已卸载系统服务（配置保留在 %s）", configPath)
	} else {
		logf("✅ 已卸载系统服务并删除配置 %s", configDir)
	}
	return nil
}

func serviceStatus() string {
	cmd := exec.Command("systemctl", "is-active", serviceName)
	out, _ := cmd.Output()
	state := strings.TrimSpace(string(out))
	if state == "" {
		state = "未安装/未知"
	}
	enabled := "否"
	cmd2 := exec.Command("systemctl", "is-enabled", serviceName)
	out2, _ := cmd2.Output()
	if strings.TrimSpace(string(out2)) == "enabled" {
		enabled = "是"
	}
	return fmt.Sprintf("服务状态: %s | 开机自启: %s", state, enabled)
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
	keyMasked := maskKey(cfg.SiliconflowAPIKey)
	fmt.Println("==================================================")
	fmt.Printf("  SiliconFlow Embedding 清洗代理 v%s\n", version)
	fmt.Println("  功能: 删除 encoding_format 后转发硅基, 解决 20015 报错")
	fmt.Println("--------------------------------------------------")
	fmt.Printf("  当前端口: %d (默认 %d)\n", cfg.Port, defaultPort)
	fmt.Printf("  硅基 Key : %s\n", keyMasked)
	fmt.Printf("  服务状态 : %s\n", status)
	fmt.Println("--------------------------------------------------")
	fmt.Println("  1) 安装为系统服务（开机自启动）")
	fmt.Println("  2) 卸载系统服务")
	fmt.Println("  3) 修改监听端口")
	fmt.Println("  4) 设置/修改 硅基 API Key")
	fmt.Println("  5) 前台启动代理（当前终端运行, Ctrl+C 停止）")
	fmt.Println("  6) 查看服务状态")
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

func interactive() {
	cfg, err := loadConfig(configPathEnv())
	if err != nil {
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
				fmt.Print("当前未设置硅基 API Key，请输入（形如 sk-xxxx）: ")
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
			del := askYesNo(reader, "是否【删除】配置文件（Key/端口将丢失）？选 n 保留配置")
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
			fmt.Print("请输入硅基 API Key（形如 sk-xxxx）: ")
			key := readLine(reader)
			if !strings.HasPrefix(key, "sk-") && key != "" {
				fmt.Println("⚠️ 输入看起来不像有效的硅基 Key（应以 sk- 开头），已保存，请确认")
			}
			if key != "" {
				cfg.SiliconflowAPIKey = key
			}
			if err := saveConfig(configPathEnv(), cfg); err != nil {
				fmt.Printf("❌ 保存失败: %v\n", err)
			} else {
				fmt.Println("✅ 硅基 API Key 已保存")
			}
		case "5":
			fmt.Println("前台启动中…（Ctrl+C 停止）")
			if err := runServer(cfg); err != nil {
				fmt.Printf("❌ 启动失败: %v\n", err)
			}
		case "6":
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
