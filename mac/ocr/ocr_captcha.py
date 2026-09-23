#!/usr/bin/env python3
# ocr_captcha.py — 用 ddddocr 识别一张验证码图片，把结果打印到 stdout。
# 用法: python3 ocr_captcha.py <图片路径>
# 依赖: pip install ddddocr（已装在本机的隔离 venv 中）
import sys

try:
    import ddddocr
except ImportError:
    print("", file=sys.stdout)
    print("ddddocr 未安装：请先 pip install ddddocr", file=sys.stderr)
    sys.exit(2)

if len(sys.argv) < 2:
    sys.exit(2)

ocr = ddddocr.DdddOcr(show_ad=False, beta=True)
with open(sys.argv[1], "rb") as f:
    result = ocr.classification(f.read())
print(result.strip())
