package main

import (
	"fmt"
	"log"
	"os"

	"cqupt/ai"
)

func main() {
	if err := ai.InitAI(); err != nil {
		log.Fatalf("InitAI 失败: %v", err)
	}
	fmt.Println("=== 测试 1：单选题 ===")
	q1 := "下列关于C语言的说法中，正确的是：\nA. C语言是面向对象的语言\nB. C语言程序必须包含一个main函数\nC. C语言不支持指针\nD. C语言不能进行位运算"
	a1, err := ai.AnswerChoice(q1)
	if err != nil {
		fmt.Printf("失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("答案 = %q\n", a1)

	fmt.Println("=== 测试 2：多空填空题 ===")
	q2 := "在C语言中，定义整型变量的关键字是▁，用▁函数可以从标准输入读取一个整数。"
	a2, err := ai.AnswerMulti(q2, 2)
	if err != nil {
		fmt.Printf("失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("答案 = %#v\n", a2)
}
