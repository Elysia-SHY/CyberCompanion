#!/usr/bin/env bash
#
# 把仓库根的 CHANGELOG.md 同步到 internal/web/static/CHANGELOG.md。
#
# Go 的 //go:embed 只能嵌入包目录内的文件，无法直接引用仓库根，
# 因此面板里的「更新日志」需要一份构建期内嵌的副本。
# 发版前运行本脚本，CI 会校验两份文件一致。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$ROOT/CHANGELOG.md"
DST="$ROOT/internal/web/static/CHANGELOG.md"

if [ ! -f "$SRC" ]; then
  echo "❌ 找不到 $SRC" >&2
  exit 1
fi

cp "$SRC" "$DST"
echo "✅ 已同步 CHANGELOG.md -> internal/web/static/CHANGELOG.md ($(wc -c <"$DST") 字节)"
