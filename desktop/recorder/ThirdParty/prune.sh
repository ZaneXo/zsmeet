#!/usr/bin/env bash
# 从 webrtc-audio-processing 和 abseil-cpp 的官方源码包里挑出录音小程序要编的文件，放到这个目录
# （desktop/recorder/ThirdParty/）下。升级版本时用它重新生成，见 README.md。
#
# 用法：desktop/recorder/ThirdParty/prune.sh <解开的 webrtc-audio-processing-2.1 目录> <解开的 abseil-cpp-20240722.1 目录>
#
# 做的事（都先在这个目录下的一个临时目录里做）：
#   1. 把源码包里的 webrtc/、absl/ 整个拷贝过来
#   2. 用 clang -MM（只做预处理：不编译、不运行源码包里的任何脚本）找出 webrtc-sources.txt 里的源文件和
#      Sources/CRecorder/zr_aec.cc、zr_agc.cc 在 Apple 芯片、Intel 上用到的全部头文件，参数和编译时一样（webrtc-flags.sh）
#   3. 只留下这些源文件、头文件和许可证，别的都删掉
#   4. 都没问题了，才用挑好的换掉这里原来的 webrtc-audio-processing/、abseil-cpp/。中途出错就停下、说明原因，
#      这里原来的文件一个都不动
# 不改任何文件的内容（没有本地补丁）。
#
# macOS 自带的 bash 3.2 也能跑（sh 也行）：不用 <(…)。bash 3.2 下在给 xargs 供数据的循环里用它，读到一半它的
# 子进程就没了，脚本一声不吭地停下；sh（POSIX 模式）干脆不认它。列表都放在变量里、用 <<< 读。
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
recorder="$(cd "$here/.." && pwd)"

if [[ $# -ne 2 ]]; then
  echo "用法：$0 <webrtc-audio-processing-2.1 目录> <abseil-cpp-20240722.1 目录>" >&2
  exit 2
fi
wap="$(cd "$1" && pwd)"
absl="$(cd "$2" && pwd)"
[[ -f "$wap/COPYING" && -d "$wap/webrtc/modules/audio_processing" ]] || { echo "$wap 不像是 webrtc-audio-processing 的源码" >&2; exit 2; }
[[ -f "$absl/LICENSE" && -d "$absl/absl/base" ]] || { echo "$absl 不像是 abseil-cpp 的源码" >&2; exit 2; }
# clang -MM 的输出用空格分隔路径
if [[ "$here" == *" "* ]]; then
  echo "这个目录的路径里有空格（${here}），换一个没有空格的地方再运行" >&2
  exit 2
fi

# shellcheck source=webrtc-flags.sh
source "$here/webrtc-flags.sh"
# 两种架构要编的源文件（「路径<TAB>额外的参数」，路径相对这个目录）
sources_arm64="$(webrtc_sources arm64)" || { echo "读不了 $here/webrtc-sources.txt" >&2; exit 1; }
sources_x86_64="$(webrtc_sources x86_64)" || { echo "读不了 $here/webrtc-sources.txt" >&2; exit 1; }

# 挑文件的临时目录（在这个目录下，最后换上去只是改个名字），和放中间结果的临时目录。结束时（包括出错、被打断）都删掉
stage="$(mktemp -d "$here/.prune.XXXXXX")"
work="$(mktemp -d)"
replaced=no
cleanup() {
  rm -rf "$stage" "$work"
}
on_error() {
  if [[ "$replaced" == no ]]; then
    echo "prune.sh 第 $1 行出错了，没做完；$here 下原来的文件没动" >&2
  else
    echo "prune.sh 第 $1 行出错了" >&2
  fi
}
trap cleanup EXIT
trap 'on_error $LINENO' ERR
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

echo "拷贝源码…"
mkdir -p "$stage/webrtc-audio-processing" "$stage/abseil-cpp"
cp -R "$wap/webrtc" "$stage/webrtc-audio-processing/webrtc"
cp "$wap/COPYING" "$wap/AUTHORS" "$wap/NEWS" "$wap/README.md" "$stage/webrtc-audio-processing/"
cp -R "$absl/absl" "$stage/abseil-cpp/absl"
cp "$absl/LICENSE" "$stage/abseil-cpp/"

# 找头文件时，webrtc_flags 给的 -I 指向临时目录里拷过来的源码（它按这个变量给路径，见 webrtc-flags.sh）
webrtc_third_party="$stage"
sdk="$(xcrun --sdk macosx --show-sdk-path)"

# 一个源文件在一个架构上用到的头文件，写到 $work/deps/ 下
for arch in arm64 x86_64; do
  flag_list="$(webrtc_flags "$arch")"
  {
    # 额外参数是「-」时表示没有（xargs -0 会丢掉空的参数）
    printf '#!/bin/sh\n# 由 prune.sh 生成：deps.sh <源文件> <输出> <额外参数或 ->\nset -e\nextra="$3"\n[ "$extra" = - ] && extra=\n'
    printf 'case "$1" in *.c) std=-std=c11; cc=clang ;; *) std=-std=c++17; cc=clang++ ;; esac\n'
    printf 'exec xcrun "$cc" "$std" -MM -isysroot %q ' "$sdk"
    while IFS= read -r flag; do printf '%q ' "$flag"; done <<<"$flag_list"
    printf -- '-I %q $extra "$1" -o "$2"\n' "$recorder/Sources/CRecorder/include"
  } >"$work/deps-$arch.sh"
  chmod +x "$work/deps-$arch.sh"
done

echo "找用到的头文件（clang -MM）…"
mkdir -p "$work/deps"
# 要做的：每个一组「脚本、源文件、输出、额外参数」，用 NUL 隔开，先都写进一个文件再交给 xargs
: >"$work/jobs"
n=0
for arch in arm64 x86_64; do
  if [[ "$arch" == arm64 ]]; then sources="$sources_arm64"; else sources="$sources_x86_64"; fi
  while IFS=$'\t' read -r src extra; do
    [[ -n "$src" ]] || continue
    n=$((n + 1))
    printf '%s\0%s\0%s\0%s\0' "$work/deps-$arch.sh" "$stage/$src" "$work/deps/$n.d" "${extra:--}" >>"$work/jobs"
  done <<<"$sources"
  # 回声消除、自动增益的 C 接口（我们自己的代码）用到的头文件也要留下
  for shim in zr_aec zr_agc; do
    n=$((n + 1))
    printf '%s\0%s\0%s\0%s\0' "$work/deps-$arch.sh" "$recorder/Sources/CRecorder/$shim.cc" "$work/deps/$n.d" - >>"$work/jobs"
  done
done
if ! xargs -0 -n 4 -P "$(sysctl -n hw.ncpu)" sh -c '"$0" "$1" "$2" "$3"' <"$work/jobs"; then
  echo "找头文件出错了（clang -MM 没通过，原因见上面的输出），$here 下原来的文件没动" >&2
  exit 1
fi
found="$(find "$work/deps" -name '*.d' | wc -l | tr -d ' ')"
if [[ "$found" != "$n" ]]; then
  echo "找头文件没做完：$n 个文件只有 $found 个找完了，$here 下原来的文件没动" >&2
  exit 1
fi

# 要留下的文件（相对这个目录）：源文件、头文件、许可证
keep="$work/keep.txt"
{
  printf '%s\n%s\n' "$sources_arm64" "$sources_x86_64" | cut -f 1
  # 依赖文件的格式：「目标: 源文件 头文件 头文件 \」，可能有好几行
  cat "$work"/deps/*.d | sed -e 's/\\$//' -e 's/^[^:]*://' | tr ' ' '\n' | sed -n "s#^$stage/##p"
  printf '%s\n' \
    webrtc-audio-processing/COPYING \
    webrtc-audio-processing/AUTHORS \
    webrtc-audio-processing/NEWS \
    webrtc-audio-processing/README.md \
    webrtc-audio-processing/webrtc/LICENSE \
    webrtc-audio-processing/webrtc/PATENTS \
    webrtc-audio-processing/webrtc/third_party/pffft/LICENSE \
    webrtc-audio-processing/webrtc/third_party/rnnoise/COPYING \
    webrtc-audio-processing/webrtc/common_audio/third_party/ooura/LICENSE \
    webrtc-audio-processing/webrtc/common_audio/third_party/spl_sqrt_floor/LICENSE \
    webrtc-audio-processing/webrtc/modules/third_party/fft/LICENSE \
    abseil-cpp/LICENSE
} | sed -e 's#/\./#/#g' -e '/^$/d' | LC_ALL=C sort -u >"$keep"

# 依赖里的路径应该都已经是规整的（没有 ..），有的话说明 include 的写法超出了预期，停下来看看
if grep -q '\.\./' "$keep"; then
  echo "依赖里有带 .. 的路径，prune.sh 处理不了（$here 下原来的文件没动）：" >&2
  grep '\.\./' "$keep" >&2
  exit 1
fi
missing="$(while IFS= read -r path; do [[ -f "$stage/$path" ]] || echo "$path"; done <"$keep")"
if [[ -n "$missing" ]]; then
  echo "这些文件在源码包里找不到（$here 下原来的文件没动）：" >&2
  echo "$missing" >&2
  exit 1
fi

echo "删掉用不到的文件…"
(cd "$stage" && find webrtc-audio-processing abseil-cpp -type f) | LC_ALL=C sort >"$work/all.txt"
LC_ALL=C comm -23 "$work/all.txt" "$keep" | while IFS= read -r path; do rm -f "$stage/$path"; done
find "$stage/webrtc-audio-processing" "$stage/abseil-cpp" -type d -empty -delete

echo "换上挑好的文件…"
replaced=yes
rm -rf "$here/webrtc-audio-processing" "$here/abseil-cpp"
mv "$stage/webrtc-audio-processing" "$stage/abseil-cpp" "$here/"

echo "留下了 $(wc -l <"$keep" | tr -d ' ') 个文件："
du -sh "$here/webrtc-audio-processing" "$here/abseil-cpp"
