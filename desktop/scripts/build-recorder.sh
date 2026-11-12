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
# 用法：desktop/scripts/build-recorder.sh [--force]    --force：不管改没改过都重新构建
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
recorder="$(cd "$here/../recorder" && pwd)"
binaries="$(cd "$here/../src-tauri" && pwd)/binaries"
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
  # 源文件、Info.plist 或者这个脚本比上次的结果新，就要重新构建
  [[ -z "$(find "$recorder/Sources" "$recorder/Info.plist" "$here/build-recorder.sh" -newer "$product" -print -quit)" ]]
}

if ! $force && up_to_date; then
  echo "录音小程序 zsmeet-recorder 已经是最新的"
  exit 0
fi

sdk="$(xcrun --sdk macosx --show-sdk-path)"
include="$recorder/Sources/CRecorder/include"

build_arch() {
  local arch="$1"
  local out="$build/$arch"
  local target="$arch-apple-macos$min_macos"
  rm -rf "$out"
  mkdir -p "$out/modules"

  # 音频线程上跑的部分（C）：环形缓冲和 IOProc
  xcrun clang -c -O2 -std=c11 -Wall -Wextra -Werror -target "$target" -isysroot "$sdk" \
    -I "$include" "$recorder/Sources/CRecorder/ring.c" -o "$out/ring.o"

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
    "$recorder/Sources/zsmeet-recorder/main.swift" "$out/libRecorderKit.a" "$out/ring.o" \
    -Xlinker -sectcreate -Xlinker __TEXT -Xlinker __info_plist -Xlinker "$recorder/Info.plist" \
    -Xlinker -dead_strip \
    -o "$out/zsmeet-recorder"
}

echo "构建录音小程序 zsmeet-recorder（Apple 芯片 + Intel）…"
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
