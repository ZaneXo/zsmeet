#!/usr/bin/env bash
# 从 webrtc-audio-processing 和 abseil-cpp 的官方源码包里挑出录音小程序要编的文件，放到这个目录
# （desktop/recorder/ThirdParty/）下。升级版本时用它重新生成，见 README.md。
#
# 用法：desktop/recorder/ThirdParty/prune.sh <解开的 webrtc-audio-processing-2.1 目录> <解开的 abseil-cpp-20240722.1 目录>
#
# 做的事：
#   1. 删掉这里原来的 webrtc-audio-processing/、abseil-cpp/，把源码包里的 webrtc/、absl/ 整个拷贝过来
#   2. 用 clang -MM（只做预处理：不编译、不运行源码包里的任何脚本）找出 webrtc-sources.txt 里的源文件和
#      Sources/CRecorder/zr_aec.cc 在 Apple 芯片、Intel 上用到的全部头文件，参数和编译时一样（webrtc-flags.sh）
#   3. 只留下这些源文件、头文件和许可证，别的都删掉
# 不改任何文件的内容（没有本地补丁）。
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

echo "拷贝源码…"
rm -rf "$here/webrtc-audio-processing" "$here/abseil-cpp"
mkdir -p "$here/webrtc-audio-processing" "$here/abseil-cpp"
cp -R "$wap/webrtc" "$here/webrtc-audio-processing/webrtc"
cp "$wap/COPYING" "$wap/AUTHORS" "$wap/NEWS" "$wap/README.md" "$here/webrtc-audio-processing/"
cp -R "$absl/absl" "$here/abseil-cpp/absl"
cp "$absl/LICENSE" "$here/abseil-cpp/"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
sdk="$(xcrun --sdk macosx --show-sdk-path)"

# 一个源文件在一个架构上用到的头文件，写到 $work/deps/ 下
for arch in arm64 x86_64; do
  {
    # 额外参数是「-」时表示没有（xargs -0 会丢掉空的参数）
    printf '#!/bin/sh\n# 由 prune.sh 生成：deps.sh <源文件> <输出> <额外参数或 ->\nset -e\nextra="$3"\n[ "$extra" = - ] && extra=\n'
    printf 'case "$1" in *.c) std=-std=c11; cc=clang ;; *) std=-std=c++17; cc=clang++ ;; esac\n'
    printf 'exec xcrun "$cc" "$std" -MM -isysroot %q ' "$sdk"
    while IFS= read -r flag; do printf '%q ' "$flag"; done < <(webrtc_flags "$arch")
    printf -- '-I %q $extra "$1" -o "$2"\n' "$recorder/Sources/CRecorder/include"
  } >"$work/deps-$arch.sh"
  chmod +x "$work/deps-$arch.sh"
done

echo "找用到的头文件（clang -MM）…"
mkdir -p "$work/deps"
jobs="$(sysctl -n hw.ncpu)"
n=0
for arch in arm64 x86_64; do
  while IFS=$'\t' read -r src extra; do
    n=$((n + 1))
    printf '%s\0%s\0%s\0%s\0' "$work/deps-$arch.sh" "$here/$src" "$work/deps/$n.d" "${extra:--}"
  done < <(webrtc_sources "$arch")
  n=$((n + 1))
  printf '%s\0%s\0%s\0%s\0' "$work/deps-$arch.sh" "$recorder/Sources/CRecorder/zr_aec.cc" "$work/deps/$n.d" -
done | xargs -0 -n 4 -P "$jobs" sh -c '"$0" "$1" "$2" "$3"'

# 要留下的文件（相对这个目录）：源文件、头文件、许可证
keep="$work/keep.txt"
{
  for arch in arm64 x86_64; do
    webrtc_sources "$arch" | cut -f 1
  done
  # 依赖文件的格式：「目标: 源文件 头文件 头文件 \」，可能有好几行
  cat "$work"/deps/*.d | sed -e 's/\\$//' -e 's/^[^:]*://' | tr ' ' '\n' | sed -n "s#^$here/##p"
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
} | sed -e 's#/\./#/#g' | sort -u >"$keep"

# 依赖里的路径应该都已经是规整的（没有 ..），有的话说明 include 的写法超出了预期，停下来看看
if grep -q '\.\./' "$keep"; then
  echo "依赖里有带 .. 的路径，prune.sh 处理不了：" >&2
  grep '\.\./' "$keep" >&2
  exit 1
fi
missing="$(while IFS= read -r path; do [[ -f "$here/$path" ]] || echo "$path"; done <"$keep")"
if [[ -n "$missing" ]]; then
  echo "这些文件在源码包里找不到：" >&2
  echo "$missing" >&2
  exit 1
fi

echo "删掉用不到的文件…"
(cd "$here" && find webrtc-audio-processing abseil-cpp -type f) | sort >"$work/all.txt"
comm -23 "$work/all.txt" "$keep" | while IFS= read -r path; do rm -f "$here/$path"; done
find "$here/webrtc-audio-processing" "$here/abseil-cpp" -type d -empty -delete

echo "留下了 $(wc -l <"$keep" | tr -d ' ') 个文件："
du -sh "$here/webrtc-audio-processing" "$here/abseil-cpp"
