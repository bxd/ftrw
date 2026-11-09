# FTRW

Telnet/SSH BBS 终端的 Wails3 (Go + Vue3) 重写版。以下约定是本项目的实际情况，修改构建/资产前请先读完。

## 构建（本项目 FTRW，先看这里）

产物一律走 wails3 的 task 流水线，不要直接 `go build`：

```
wails3 task build   # 输出 bin/ftrw.exe
wails3 task run     # 运行它
```

直接 `go build -o fqterm.exe .` 会踩两个已经真实发生过的坑：

1. 少了 `-ldflags="-w -s -H windowsgui"`，链接成控制台子系统（PE Subsystem=3），启动时会闪一个 CMD 窗口；
2. 少了 `wails3 generate syso`，exe 里没有图标 / manifest / 版本信息；而且产物名与目录本就由根 `Taskfile.yml` 的 `APP_NAME` / `BIN_DIR` 决定（当前 `ftrw` / `bin`），手 build 出来的 `fqterm.exe` 只会落在源码目录里。

图标唯一源文件是 `build/appicon.png`，托盘图标是 `build/tray/tray-{light,dark}bg.ico`，两者都由 `go run ./cmd/genicon` 确定性生成（见该文件头部用法）。禁止运行 `wails3 task common:update:build-assets`：它会用模板反向覆盖 `build/config.yml`、`build/windows/info.json` 等资产。

## 日志

因为产物是 GUI 子系统（见上一节第 1 点），双击启动时没有控制台，`log.Printf` 无处可去。所以启动时会把标准库日志接到滚动文件：

```
%APPDATA%\FTRW\logs\ftrw.log          当前日志（单文件 4MB 上限）
%APPDATA%\FTRW\logs\ftrw-<时间戳>.log  历史日志，保留 4 份
```

Wails 内部的 `log.Printf`（如 `[WebView2] ...`）与系统 slog 都会落在这里；从终端用 `wails3 task run` 跑时依旧同时输出到 stderr。托盘菜单有「打开日志目录」一项可直接到达。滚动策略在 `logging.go`，回归测试见 `logging_test.go`（其中一条专门守“rotate 里误调 log.Printf 导致自锁”）。

## 开发模式

```
wails3 task run     # 构建后跑 bin/ftrw.exe
wails3 dev          # 前后端热重载；正式产物仍须走 wails3 task build
```

后端接口靠前端手写 TS 调用（`frontend/src/api.ts` 里用 `Call.ByName`），不依赖 `frontend/bindings/`；该目录每次 build 自动重生成，已进 .gitignore。

前端只留 `index.html` 真正引用的静态资源（`public/style.css`、`public/wails.png`）。Wails 模板自带的演示图 bg-desktop.jpg / bg-mobile.jpg / vue.svg、没有任何 @font-face 引用它们的 Inter-Medium.ttf 与对应许可证文件、以及模板写的 `frontend/README.md` 均已删除，需要时从模板重取。

## 测试

```
cd fqterm
gofmt -l .; go vet .; go test -count=1 .   # 只关注自己改的文件时，gofmt -l 可接文件名
```

`go test -c` 会留下 `fqterm_test.exe` 一类产物，已走 `*.exe` 忽略规则，不必手动处理。涉及 ConPTY 的用例对运行环境敏感（子进程、伪终端），失败时先看是不是被残留的 shell 进程拖住。
