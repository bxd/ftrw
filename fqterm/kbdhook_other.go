//go:build !windows

package main

// 跨应用热键的低级键盘钩子目前只有 Windows 实现（WH_KEYBOARD_LL）。
//
// 其它平台要各自补：macOS 用 CGEventTap + 辅助功能授权，Linux 用 X11 全局键盘 grab
// （Wayland 干脆不开放这个能力）。这里给一个不改调用方形状的桩：AppService 按
// kbdHookSupported 打一条明确的日志，用户不会以为「配了热键却没生效」是界面坏了。
const kbdHookSupported = false

type kbdHook struct {
	onEvent func(gkEvent) bool
}

func startKbdHook(onEvent func(gkEvent) bool) (*kbdHook, error) {
	return &kbdHook{onEvent: onEvent}, nil
}

func (h *kbdHook) stop() {}
