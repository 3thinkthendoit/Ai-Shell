#!/usr/bin/env bash
#
# 在本机跑 `go test -race` 的封装。
#
# 背景（本机特有的坑）：
#   本机只装了 MinGW 8.1.0。Go 的 race 运行时只有在包**启用了 cgo** 时才能被正确
#   链接进去，否则测试二进制在加载阶段就失败：
#       exit status 0xc0000139   (STATUS_ENTRYPOINT_NOT_FOUND)
#   而 cgo 不能写在 _test.go 里 —— Go 会直接报 "use of cgo in test not supported"。
#   所以这里临时给每个包注入一个带 `//go:build race` 标签的 cgo 文件，跑完立刻删除。
#   因为有构建标签，普通 `go build` / `go test` 完全不受影响。
#
# 用法：scripts/race.sh [额外的 go test 参数]
#   ./scripts/race.sh                 # 全量
#   ./scripts/race.sh ./internal/sshclient/ -run TestConcurrent -v

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# MinGW 默认不在 PATH 里，先找一下
if ! command -v gcc >/dev/null 2>&1; then
  for c in /d/soft/mingw64/bin /c/mingw64/bin /c/msys64/mingw64/bin /c/msys64/usr/bin; do
    if [ -x "$c/gcc.exe" ] || [ -x "$c/gcc" ]; then
      PATH="$c:$PATH"
      break
    fi
  done
fi

if ! command -v gcc >/dev/null 2>&1; then
  echo "找不到 gcc —— -race 需要 CGO。请先安装 mingw-w64。" >&2
  exit 1
fi

export CGO_ENABLED=1

MOD="$(go list -m)"
FILES=()

# 注意：用**相对路径**记录要删除的文件。
# `go list -f '{{.Dir}}'` 返回的是 Windows 绝对路径（带盘符与反斜杠），
# 直接拼给 rm 会被安全删除层误解析成 "D:\...\D:\..." 而失败；
# 更糟的是在 set -e 下，清理循环会在第一个文件上中断，留下全部残留。
cleanup() {
  local f
  for f in ${FILES[@]+"${FILES[@]}"}; do
    [ -n "$f" ] && rm -f "$f"
  done
}

while IFS= read -r imp; do
  [ -n "$imp" ] || continue
  if [ "$imp" = "$MOD" ]; then
    rel="."
  else
    rel="${imp#"$MOD"/}"
  fi
  [ -d "$rel" ] || continue
  # 只处理确实含有非测试 Go 文件的包
  ls "$rel"/*.go >/dev/null 2>&1 || continue

  # 用 import path 而不是相对目录：`go list internal/agent` 会被当成
  # 「标准库里的 internal/agent」而报错，必须写成 `./internal/agent`。
  # 既然循环里拿到的本来就是 import path，直接用它最省事。
  pkgname="$(go list -f '{{.Name}}' "$imp" 2>/dev/null || true)"
  [ -n "$pkgname" ] || continue

  f="$rel/zz_race_enabler.go"
  cat > "$f" <<EOF
//go:build race

// 由 scripts/race.sh 临时生成，运行结束会自动删除。
package $pkgname

// #include <stdlib.h>
import "C"

func zzRaceEnabler() { C.free(nil) }
EOF
  FILES+=("$f")
done < <(go list ./... 2>/dev/null)

trap cleanup EXIT INT TERM

# 默认全量；若调用方自己指定了包（形如 ./internal/x），就不再追加 ./...，
# 否则两种写法会叠加成「跑全部包但只过滤某个 -run」，与直觉不符。
PKGS=0
for a in "$@"; do
  case "$a" in
    -*) ;;
    */*|.) PKGS=1 ;;
  esac
done
if [ "$PKGS" -eq 0 ]; then
  set -- ./... "$@"
fi

echo "已为 ${#FILES[@]} 个包注入 cgo 使能文件，开始 -race ..."
# -count=1：禁用测试缓存。验证性跑测必须真的执行一遍 ——
# 否则「全绿」可能只是上一次运行的残留结果，起不到验证作用。
go test -race -count=1 "$@"
