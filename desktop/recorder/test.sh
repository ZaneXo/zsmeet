#!/usr/bin/env bash
# 录音小程序的测试：先构建要打包的程序（desktop/scripts/build-recorder.sh），再编译测试程序
# （Tests/recorder-tests，不需要 XCTest）并运行：
#   - 单元测试：事件格式、参数、ADTS、分片、混音、编码、暂停……
#   - 端到端：真的跑 zsmeet-recorder，用 --input-file 代替麦克风和电脑声音（不要权限、不弹窗），
#     再用 ffmpeg、ffprobe 检查分片拼起来能解码、时长和录到的对得上。
# 需要 ffmpeg 和 ffprobe（brew install ffmpeg）。
#
# 用法：desktop/recorder/test.sh
#       只跑名字里有某几个字的测试：ZSMEET_RECORDER_TEST_FILTER=暂停 desktop/recorder/test.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
"$here/../scripts/build-recorder.sh"

# Homebrew 装的 ffmpeg 可能不在 PATH 里
export PATH="$PATH:/opt/homebrew/bin:/usr/local/bin"
for tool in ffmpeg ffprobe; do
  if ! command -v "$tool" >/dev/null; then
    echo "测试需要 $tool：brew install ffmpeg" >&2
    exit 1
  fi
done

sdk="$(xcrun --sdk macosx --show-sdk-path)"
arch="$(uname -m)"
target="$arch-apple-macos14.4"
include="$here/Sources/CRecorder/include"
out="$here/.build/tests-$arch"
rm -rf "$out"
mkdir -p "$out/modules"

echo "编译测试…"
xcrun clang -c -g -std=c11 -Wall -Wextra -Werror -target "$target" -isysroot "$sdk" \
  -I "$include" "$here/Sources/CRecorder/ring.c" -o "$out/ring.o"
xcrun swiftc -Onone -g -enable-testing -swift-version 5 -target "$target" -sdk "$sdk" \
  -parse-as-library -module-name RecorderKit -emit-library -static \
  -emit-module -emit-module-path "$out/modules/RecorderKit.swiftmodule" \
  -I "$include" -Xcc -fmodule-map-file="$include/module.modulemap" \
  "$here"/Sources/RecorderKit/*.swift -o "$out/libRecorderKit.a"
xcrun swiftc -Onone -g -swift-version 5 -target "$target" -sdk "$sdk" -module-name recorder_tests \
  -I "$out/modules" -I "$include" -Xcc -fmodule-map-file="$include/module.modulemap" \
  "$here"/Tests/recorder-tests/*.swift "$out/libRecorderKit.a" "$out/ring.o" \
  -o "$out/recorder-tests"

echo "运行测试…"
ZSMEET_RECORDER="$here/.build/release-universal/zsmeet-recorder" "$out/recorder-tests"
