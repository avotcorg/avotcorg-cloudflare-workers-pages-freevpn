package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/gorilla/websocket"
	"golang.org/x/sys/windows/registry"
	utls "github.com/refraction-networking/utls"
)

var (
	logBox    *widget.Entry
	logScroll *container.Scroll
	proxyStop chan struct{}
	cfg       *Config
)

type Config struct {
	Port      int    `json:"port"`
	Password  string `json:"password"`
	WssHost   string `json:"wss"`
	ChunkSize int    `json:"chunk"`
}

func loadConfig() *Config {
	path := filepath.Join("otc", "config.json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		defaultCfg := &Config{
			Port:      9090,
			Password:  "otc",
			WssHost:   "kaiche1.pages.dev",
			ChunkSize: 64,
		}
		saveConfig(defaultCfg)
		return defaultCfg
	}
	data, _ := os.ReadFile(path)
	var c Config
	_ = json.Unmarshal(data, &c)
	return &c
}

func saveConfig(c *Config) {
	os.MkdirAll("otc", 0755)
	data, _ := json.MarshalIndent(c, "", "  ")
	_ = os.WriteFile(filepath.Join("otc", "config.json"), data, 0644)
}

func appendLog(msg string) {
	now := time.Now().Format("2006-01-02 15:04:05")
	fullMsg := fmt.Sprintf("[%s] %s\n", now, msg)
	logBox.SetText(logBox.Text + fullMsg)
	logScroll.ScrollToBottom()
	// 写入每日日志
	os.MkdirAll("otc/logs", 0755)
	logFile := filepath.Join("otc/logs", "proxy-"+time.Now().Format("20060102")+".log")
	f, _ := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	defer f.Close()
	f.WriteString(fullMsg)
}

func setAutoStart(enable bool) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	exePath, _ := os.Executable()
	if enable {
		return key.SetStringValue("GoWSProxy-OTC", exePath)
	} else {
		return key.DeleteValue("GoWSProxy-OTC")
	}
}

func isAutoStart() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	_, _, err = key.GetStringValue("GoWSProxy-OTC")
	return err == nil
}

func Debug(err error) {
	if err != nil {
		appendLog("[ERROR] " + err.Error())
	}
}

func utlsDialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	tcpConn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	uconn := utls.UClient(tcpConn, &utls.Config{ServerName: host}, utls.HelloRandomized)
	if dl, ok := ctx.Deadline(); ok {
		_ = uconn.SetDeadline(dl)
	}
	if err := uconn.Handshake(); err != nil {
		tcpConn.Close()
		return nil, err
	}
	_ = uconn.SetDeadline(time.Time{})
	return uconn, nil
}

func PipeConn(ws *websocket.Conn, conn net.Conn, chunkSize int) {
	buf := make([]byte, chunkSize*1024)
	go func() {
		defer conn.Close()
		for {
			mt, r, err := ws.NextReader()
			if err != nil {
				Debug(err)
				return
			}
			if mt != websocket.BinaryMessage {
				io.Copy(io.Discard, r)
				continue
			}
			if _, err := io.CopyBuffer(conn, r, buf); err != nil {
				Debug(err)
				return
			}
		}
	}()
	for {
		n, err := conn.Read(buf)
		if err != nil {
			Debug(err)
			return
		}
		if n > 0 {
			w, err := ws.NextWriter(websocket.BinaryMessage)
			if err != nil {
				Debug(err)
				return
			}
			if _, err = w.Write(buf[:n]); err != nil {
				Debug(err)
				w.Close()
				return
			}
			w.Close()
		}
	}
}

func SetUpTunnel(client net.Conn, target string, c *Config) {
	defer client.Close()
	header := make(http.Header)
	header.Set("X-Target", target)
	header.Set("X-Password", c.Password)
	dialer := websocket.Dialer{
		NetDialTLSContext: utlsDialTLSContext,
		HandshakeTimeout:  30 * time.Second,
	}
	ws, resp, err := dialer.Dial("wss://"+c.WssHost, header)
	if err != nil {
		Debug(err)
		if resp != nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			appendLog("连接websocket出错: " + string(body))
		}
		return
	}
	defer ws.Close()
	PipeConn(ws, client, c.ChunkSize)
}

func StartProxy(c *Config) {
	proxyStop = make(chan struct{})
	addr := fmt.Sprintf(":%d", c.Port)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "不支持 Hijacking", http.StatusInternalServerError)
			return
		}
		client, _, err := hijacker.Hijack()
		if err != nil {
			Debug(err)
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		appendLog("访问: " + r.Host)
		go SetUpTunnel(client, r.Host, c)
	})
	server := &http.Server{
		Addr:    addr,
		Handler: handler,
	}
	appendLog(fmt.Sprintf("OTC友情提醒 TG 频道 @soqunla HTTP代理启动，端口: %d", c.Port))
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			Debug(err)
		}
	}()
	<-proxyStop
	server.Shutdown(context.Background())
	appendLog("代理已停止")
}

func StopProxy() {
	if proxyStop != nil {
		close(proxyStop)
	}
}

func main() {
	cfg = loadConfig()
	a := app.NewWithID("OTC-Proxy TG 频道 @soqunla")
	a.Settings().SetTheme(theme.LightTheme()) // 确保使用软件渲染兼容性
	w := a.NewWindow("Go Fyne WebSocket 代理")
	w.Resize(fyne.NewSize(600, 500))

	// 托盘
	trayMenu := fyne.NewMenu("代理控制",
		fyne.NewMenuItem("显示窗口", func() { w.Show() }),
		fyne.NewMenuItem("退出程序", func() { StopProxy(); a.Quit() }),
	)
	a.SetSystemTrayMenu(trayMenu)
	a.SetIcon(theme.ComputerIcon())

	w.SetCloseIntercept(func() { w.Hide(); appendLog("[INFO] 已最小化到托盘") })

	portEntry := widget.NewEntry()
	portEntry.SetText(strconv.Itoa(cfg.Port))
	passwdEntry := widget.NewEntry()
	passwdEntry.SetText(cfg.Password)
	wssEntry := widget.NewEntry()
	wssEntry.SetText(cfg.WssHost)
	chunkEntry := widget.NewEntry()
	chunkEntry.SetText(strconv.Itoa(cfg.ChunkSize))

	logBox = widget.NewMultiLineEntry()
	logBox.SetPlaceHolder("日志输出...")
	logScroll = container.NewScroll(logBox)
	logScroll.SetMinSize(fyne.NewSize(580, 300))

	autoStartCheck := widget.NewCheck("开机自启动", func(checked bool) {
		err := setAutoStart(checked)
		if err != nil {
			appendLog("[ERROR] 设置自启动失败: " + err.Error())
		} else {
			appendLog("[INFO] 开机自启动已 " + map[bool]string{true: "启用", false: "关闭"}[checked])
		}
	})
	autoStartCheck.SetChecked(isAutoStart())

	startBtn := widget.NewButton("启动代理", func() {
		cfg.Port, _ = strconv.Atoi(portEntry.Text)
		cfg.Password = passwdEntry.Text
		cfg.WssHost = wssEntry.Text
		cfg.ChunkSize, _ = strconv.Atoi(chunkEntry.Text)
		saveConfig(cfg)
		go StartProxy(cfg)
	})
	stopBtn := widget.NewButton("停止代理", StopProxy)

	form := container.NewVBox(
		widget.NewForm(
			widget.NewFormItem("端口", portEntry),
			widget.NewFormItem("密码", passwdEntry),
			widget.NewFormItem("WSS 地址", wssEntry),
			widget.NewFormItem("分片大小 (KB)", chunkEntry),
		),
		autoStartCheck,
		container.NewHBox(startBtn, stopBtn),
		logScroll,
	)

	w.SetContent(form)
	w.ShowAndRun()
}