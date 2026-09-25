package main

import (
	"testing"

	"cqupt/train"
)

// 交互式输入的解析是最容易在边界上翻车的地方：用户多打一个空格、
// 直接敲回车、或者输个负数，都不该让程序带着坏参数跑起来。
func TestParseNum(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"3", 3, false},
		{" 5 ", 5, false},   // 前后空格
		{"\t7\n", 7, false}, // 制表符与换行
		{"1", 1, false},
		{"", 0, true},                     // 直接回车
		{"   ", 0, true},                  // 全空白
		{"abc", 0, true},                  // 非数字
		{"3.5", 0, true},                  // 小数
		{"-1", 0, true},                   // 负数
		{"0", 0, true},                    // 零不是合法题量
		{"99999999999999999999", 0, true}, // 溢出
	}
	for _, c := range cases {
		got, err := parseNum(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseNum(%q) 应报错，却返回 %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseNum(%q) 意外报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseNum(%q) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

// 进度事件回调必须能安全处理所有事件类型，包括没见过的类型。
// 终端输出是最外层的东西，它崩了会把整个刷题过程带下去。
func TestPrintEventHandlesAllKinds(t *testing.T) {
	kinds := []string{
		train.EventStarted, train.EventLoginOK, train.EventAssignmentIn,
		train.EventQuestionDone, train.EventManualLogin, train.EventFinished,
		"某个以后才会加的新类型", "",
	}
	for _, k := range kinds {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("printEvent 处理 %q 时 panic: %v", k, r)
				}
			}()
			printEvent(train.Event{Kind: k, Pid: "answerForm1", Score: 2, Detail: "x"})
		}()
	}
}

// modeFlag 的默认值必须是 quiz —— 用户直接 go run . 时走的仍是选择题流程。
func TestModeFlagDefaultsToQuiz(t *testing.T) {
	if *modeFlag != train.ModeQuiz {
		t.Errorf("mode 默认值 = %q，期望 %q", *modeFlag, train.ModeQuiz)
	}
}
