package main

import (
	"reflect"
	"testing"
)

// fmstate 往返：SetFMState 落盘、新 Store 原样读回；缺文件是零值（不影响启动）。
// 前端防抖调 SetFMState，启动 GetFMState 还原标签/双栏，形状不能漂。

func TestFMStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := &Store{dir: dir}
	if err := s.load(); err != nil {
		t.Fatal(err)
	}
	if len(s.FMState.Tabs) != 0 {
		t.Fatalf("空目录应无会话：%+v", s.FMState)
	}

	want := FMState{
		Tabs: []FMTabState{
			{Panes: []string{`C:\`, `D:\work`}, Active: 1},
			{Panes: []string{`E:\pack.zip::sub`, `C:\Users`}, Active: 0},
		},
		Active: 1,
	}
	cs := NewConfigService(s)
	if err := cs.SetFMState(want); err != nil {
		t.Fatal(err)
	}

	s2 := &Store{dir: dir}
	if err := s2.load(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s2.FMState, want) {
		t.Fatalf("往返不一致：\n got %+v\nwant %+v", s2.FMState, want)
	}
	cs2 := NewConfigService(s2)
	if got := cs2.GetFMState(); !reflect.DeepEqual(got, want) {
		t.Fatalf("GetFMState 不一致：%+v", got)
	}

	// 活动序号越界钳制：-1 是「活动的不是文件标签」的合法值，更小的当 -1。
	if err := cs2.SetFMState(FMState{Active: -5}); err != nil {
		t.Fatal(err)
	}
	if got := cs2.GetFMState(); got.Active != -1 {
		t.Fatalf("Active 应钳到 -1，得 %d", got.Active)
	}
}
