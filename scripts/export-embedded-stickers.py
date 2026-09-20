#!/usr/bin/env python3
"""从 internal/stickers/stickers.go 的 base64 字面量里导出内置表情图片。

这个脚本是一次性的迁移工具：原实现把 5 张表情图片以 base64 字符串
硬编码在 Go 源码里（整个文件 801KB），改为图床方案后这些字节应该
落成真实的图片文件，方便用户批量上传到自己的图床。
"""
import base64
import os
import re
import sys

SRC = sys.argv[1] if len(sys.argv) > 1 else "internal/stickers/stickers.go"
OUT = sys.argv[2] if len(sys.argv) > 2 else "stickers-export"

with open(SRC, "r", encoding="utf-8") as f:
    lines = f.read().split("\n")

# 结构形如：
#     "love": {
#         "<base64>",
#     },
scene_re = re.compile(r'^\s*"([A-Za-z0-9_]+)":\s*\{\s*$')
blob_re = re.compile(r'^\s*"([A-Za-z0-9+/=]{200,})",?\s*$')

os.makedirs(OUT, exist_ok=True)

current_scene = None
found = []
index_by_scene = {}

for line in lines:
    m = scene_re.match(line)
    if m:
        current_scene = m.group(1)
        continue
    b = blob_re.match(line)
    if b and current_scene:
        raw = base64.b64decode(b.group(1), validate=True)
        index_by_scene[current_scene] = index_by_scene.get(current_scene, 0) + 1
        n = index_by_scene[current_scene]

        if raw[:2] == b"\xff\xd8":
            ext = "jpg"
            kind = "JPEG"
        elif raw[:4] == b"RIFF" and raw[8:12] == b"WEBP":
            ext = "webp"
            kind = "WebP"
        elif raw[:8] == b"\x89PNG\r\n\x1a\n":
            ext = "png"
            kind = "PNG"
        elif raw[:3] == b"GIF":
            ext = "gif"
            kind = "GIF"
        else:
            ext = "bin"
            kind = "unknown"

        name = f"{current_scene}_{n}.{ext}"
        path = os.path.join(OUT, name)
        with open(path, "wb") as g:
            g.write(raw)
        found.append((name, kind, len(raw)))
        current_scene = None

print(f"导出目录: {OUT}")
print(f"共导出 {len(found)} 张:")
for name, kind, size in found:
    print(f"  {name:20s} {kind:8s} {size:>9,d} bytes")
