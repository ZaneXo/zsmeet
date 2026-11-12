#!/usr/bin/env bash
# 构建录音小程序 zsmeet-recorder（Swift，desktop/recorder/），放到 Tauri 打包要的位置：
#   desktop/src-tauri/binaries/zsmeet-recorder-<目标三元组>
# Apple 芯片和 Intel 各编一次，用 lipo 合成一个通用（universal）的文件，三种三元组的名字各放一份：
# tauri dev、tauri build（本机架构）和 tauri build --target universal-apple-darwin 用的都是它。
# Tauri 打包时把它放进 App 的 Contents/MacOS/，用 App 的身份重新签名，带上 hardened runtime 和
# entitlements.plist（麦克风权限）。检查：codesign -d --entitlements - zsmeet.app/Contents/MacOS/zsmeet-recorder
#
# 直接用 swiftc 编译，不用 swift build：只装了 Command Line Tools 时 swift build 不能一次编两种架构，
# 有的机器上残留的旧版本清单接口还会让它读不了 Package.swift。源文件的目录和 Package.swift 里的一致。
# 源文件没改过时直接跳过（tauri dev、tauri build 每次都会调用它）。
#
# 回声消除用的 WebRTC 音频处理（desktop/recorder/ThirdParty/，几百个 C/C++ 文件）每种架构编成一个静态库
# desktop/recorder/.build/webrtc-<架构>/libwebrtc_apm.a。第一次（或者第三方源码、编译参数、编译器、SDK 变了以后）
# 要编几分钟，之后直接用：按这些东西的内容算一个指纹，对得上就不重编（和文件的修改时间无关，CI 缓存这个目录就行）。
# 编到一半被打断了，下次接着编没编完的文件。只要 Command Line Tools，不联网，不要 meson、ninja、cmake。
#
# 用法：desktop/scripts/build-recorder.sh [--force]    --force：不管改没改过都重新构建（WebRTC 的静态库照样按指纹判断）
# 用 sh 运行也行（sh desktop/scripts/build-recorder.sh）：macOS 的 sh 是 POSIX 模式的 bash 3.2，不认 <(…)，
# 所以这里先把命令的输出存进变量，再用 <<< 一行一行地读。
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
recorder="$(cd "$here/../recorder" && pwd)"
binaries="$(cd "$here/../src-tauri" && pwd)/binaries"
third_party="$recorder/ThirdParty"
build="$recorder/.build/release-universal"
product="$build/zsmeet-recorder"
triples=(aarch64-apple-darwin x86_64-apple-darwin universal-apple-darwin)
# 和 tauri.conf.json 里的 minimumSystemVersion 一致
min_macos=14.4

force=false
case "${1:-}" in
  "") ;;
  --force) force=true ;;
  *) echo "用法：$0 [--force]" >&2; exit 2 ;;
esac

up_to_date() {
  [[ -f "$product" ]] || return 1
  local triple
  for triple in "${triples[@]}"; do
    cmp -s "$product" "$binaries/zsmeet-recorder-$triple" || return 1
  done
  # 源文件、第三方源码（和它的编译参数）、Info.plist 或者这个脚本比上次的结果新，就要重新构建
  [[ -z "$(find "$recorder/Sources" "$third_party/webrtc-audio-processing" "$third_party/abseil-cpp" "$third_party/webrtc-flags.sh" \
    "$third_party/webrtc-sources.txt" "$recorder/Info.plist" "$here/build-recorder.sh" -newer "$product" -print -quit)" ]]
}

if ! $force && up_to_date; then
  echo "录音小程序 zsmeet-recorder 已经是最新的"
  exit 0
fi

sdk="$(xcrun --sdk macosx --show-sdk-path)"
include="$recorder/Sources/CRecorder/include"
jobs="$(sysctl -n hw.ncpu 2>/dev/null || echo 4)"
# shellcheck source=../recorder/ThirdParty/webrtc-flags.sh
source "$third_party/webrtc-flags.sh"

# 第三方源码和编译参数（webrtc-flags.sh、webrtc-sources.txt）的内容摘要，两种架构共用。
# README.md、prune.sh 改了不用重编
third_party_digest() {
  (cd "$third_party" &&
    find webrtc-audio-processing abseil-cpp webrtc-flags.sh webrtc-sources.txt -type f ! -name .DS_Store -print0 |
    LC_ALL=C sort -z | xargs -0 shasum -a 256) | shasum -a 256 | cut -c1-64
}

# 编 WebRTC 音频处理和 abseil，打成 .build/webrtc-<架构>/libwebrtc_apm.a。在 build_arch 之前调用
build_webrtc() {
  local arch="$1" digest="$2"
  local dir="$recorder/.build/webrtc-$arch"
  local objects="$dir/obj"
  local library="$dir/libwebrtc_apm.a"
  mkdir -p "$dir"

  # 编一个文件的脚本：compile.sh <源文件> <目标文件> <这个文件额外的参数，没有时是「-」>。
  # 参数都写死在里面，所以它的内容（连同编译器、SDK 的版本和源码的摘要）就是这批目标文件的指纹
  {
    printf '#!/bin/sh\n# 由 build-recorder.sh 生成，别手改\nset -e\nextra="$3"\n[ "$extra" = - ] && extra=\n'
    printf 'case "$1" in *.c) cc=clang; std=-std=c11 ;; *) cc=clang++; std=-std=c++17 ;; esac\n'
    # -w：第三方代码的警告不看（不改它们的代码）
    printf 'xcrun "$cc" "$std" -c -w -isysroot %q ' "$sdk"
    local flag flag_list
    flag_list="$(webrtc_flags "$arch")"
    while IFS= read -r flag; do printf '%q ' "$flag"; done <<<"$flag_list"
    printf '$extra "$1" -o "$2.tmp"\nmv -f "$2.tmp" "$2"\n'
  } >"$dir/compile.sh.new"
  chmod +x "$dir/compile.sh.new"
  local fingerprint
  fingerprint="$({
    cat "$dir/compile.sh.new"
    webrtc_sources "$arch"
    xcrun clang --version
    xcrun --sdk macosx --show-sdk-version
    echo "$digest"
  } | shasum -a 256 | cut -c1-64)"

  if [[ -f "$library" && "$(cat "$dir/library.fingerprint" 2>/dev/null)" == "$fingerprint" ]]; then
    rm -f "$dir/compile.sh.new"
    echo "WebRTC 音频处理（${arch}）已经编好了"
    return
  fi
  # 已经编好的目标文件是别的源码、参数编出来的：全部重编
  if [[ "$(cat "$dir/objects.fingerprint" 2>/dev/null)" != "$fingerprint" ]]; then
    rm -rf "$objects" "$library" "$dir/library.fingerprint"
    mkdir -p "$objects"
    echo "$fingerprint" >"$dir/objects.fingerprint"
  fi
  mv -f "$dir/compile.sh.new" "$dir/compile.sh"

  # 目标文件按源文件的完整路径命名（/ 换成 __），免得静态库里有同名的成员
  local src extra obj sources
  local list="$dir/objects.txt" queue="$dir/queue"
  sources="$(webrtc_sources "$arch")"
  : >"$list"
  : >"$queue"
  while IFS=$'\t' read -r src extra; do
    [[ -n "$src" ]] || continue
    obj="$objects/${src//\//__}.o"
    echo "$obj" >>"$list"
    [[ -f "$obj" ]] || printf '%s\0%s\0%s\0' "$third_party/$src" "$obj" "${extra:--}" >>"$queue"
  done <<<"$sources"

  local total pending
  total="$(wc -l <"$list" | tr -d ' ')"
  pending="$(tr -cd '\0' <"$queue" | wc -c | tr -d ' ')"
  pending=$((pending / 3))
  if ((pending > 0)); then
    echo "编译 WebRTC 音频处理（${arch}）：$pending / $total 个文件，第一次要几分钟…"
    if ! xargs -0 -n 3 -P "$jobs" "$dir/compile.sh" <"$queue"; then
      echo "编译 WebRTC 音频处理（${arch}）失败，错误见上面。编好的文件留着，修好以后再运行会接着编" >&2
      exit 1
    fi
  fi
  # 编译进程被系统杀掉时 xargs 不一定报错：一个个确认都编出来了
  local missing=0
  while IFS= read -r obj; do
    [[ -f "$obj" ]] || missing=$((missing + 1))
  done <"$list"
  if ((missing > 0)); then
    echo "编译 WebRTC 音频处理（${arch}）时有 $missing 个文件没编出来（编译进程可能被打断了），再运行一次会接着编" >&2
    exit 1
  fi
  rm -f "$queue"
  libtool -static -no_warning_for_no_symbols -o "$library.tmp" -filelist "$list"
  mv -f "$library.tmp" "$library"
  echo "$fingerprint" >"$dir/library.fingerprint"
}

# 回声消除的 C 接口（zr_aec.cc，C++）要用的参数：宏和 WebRTC 一样（不一样的话头文件里的结构体可能对不上），
# 第三方的头文件当成系统头文件（-isystem），它们的警告不算，我们自己的代码照样 -Wall -Wextra -Werror
shim_flags() {
  webrtc_flags "$1" | sed 's/^-I$/-isystem/'
}

build_arch() {
  local arch="$1"
  local out="$build/$arch"
  local target="$arch-apple-macos$min_macos"
  rm -rf "$out"
  mkdir -p "$out/modules"

  # 音频线程上跑的部分（C）：环形缓冲和 IOProc
  xcrun clang -c -O2 -std=c11 -Wall -Wextra -Werror -target "$target" -isysroot "$sdk" \
    -I "$include" "$recorder/Sources/CRecorder/ring.c" -o "$out/ring.o"

  # 回声消除的 C 接口
  local flags=()
  local flag flag_list
  flag_list="$(shim_flags "$arch")"
  while IFS= read -r flag; do flags+=("$flag"); done <<<"$flag_list"
  xcrun clang++ -c -std=c++17 -Wall -Wextra -Werror -isysroot "$sdk" "${flags[@]}" \
    -I "$include" "$recorder/Sources/CRecorder/zr_aec.cc" -o "$out/zr_aec.o"

  # RecorderKit：除了入口以外的 Swift 代码，编成静态库
  xcrun swiftc -O -wmo -swift-version 5 -target "$target" -sdk "$sdk" \
    -parse-as-library -module-name RecorderKit -emit-library -static \
    -emit-module -emit-module-path "$out/modules/RecorderKit.swiftmodule" \
    -I "$include" -Xcc -fmodule-map-file="$include/module.modulemap" \
    "$recorder"/Sources/RecorderKit/*.swift -o "$out/libRecorderKit.a"

  # 程序本身。Info.plist 嵌进 __TEXT,__info_plist，签名时用里面的标识
  xcrun swiftc -O -swift-version 5 -target "$target" -sdk "$sdk" \
    -module-name zsmeet_recorder \
    -I "$out/modules" -I "$include" -Xcc -fmodule-map-file="$include/module.modulemap" \
    "$recorder/Sources/zsmeet-recorder/main.swift" "$out/libRecorderKit.a" "$out/ring.o" "$out/zr_aec.o" \
    "$recorder/.build/webrtc-$arch/libwebrtc_apm.a" -lc++ -framework CoreFoundation \
    -Xlinker -sectcreate -Xlinker __TEXT -Xlinker __info_plist -Xlinker "$recorder/Info.plist" \
    -Xlinker -dead_strip \
    -o "$out/zsmeet-recorder"
}

echo "构建录音小程序 zsmeet-recorder（Apple 芯片 + Intel）…"
digest="$(third_party_digest)"
build_webrtc arm64 "$digest"
build_webrtc x86_64 "$digest"
build_arch arm64
build_arch x86_64
lipo -create -output "$product.tmp" "$build/arm64/zsmeet-recorder" "$build/x86_64/zsmeet-recorder"
# 先给一个临时的签名（ad-hoc），tauri dev 时直接就能跑；打包时 Tauri 会重新签
codesign --force --sign - --identifier io.github.zanexo.zsmeet.recorder "$product.tmp" 2>/dev/null
mv -f "$product.tmp" "$product"

mkdir -p "$binaries"
for triple in "${triples[@]}"; do
  cp -f "$product" "$binaries/zsmeet-recorder-$triple"
done
echo "录音小程序已放到 $binaries/zsmeet-recorder-{$(IFS=,; echo "${triples[*]}")}"
