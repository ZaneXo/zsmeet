#!/usr/bin/env bash
# 录音小程序的测试：先构建要打包的程序（desktop/scripts/build-recorder.sh），再编译测试程序
# （Tests/recorder-tests，不需要 XCTest）并运行：
#   - 单元测试：事件格式、参数、ADTS、分片、混音、编码、暂停、回声消除这一步、麦克风自动增益这一步……
#   - 回声消除（AEC3）消得好不好：合成的像说话的声音，量回声消掉了多少、这边说的话保留得怎么样（不放声音、不要设备）
#   - 麦克风自动增益（AGC2）调得对不对：合成的小声、大声的说话和底噪，量调到了多大、有没有把底噪调大、会不会碰顶
#   - 端到端：真的跑 zsmeet-recorder，用 --input-file 代替麦克风和电脑声音（不要权限、不弹窗），
#     再用 ffmpeg、ffprobe 检查分片拼起来能解码、时长和录到的对得上。
# 需要 ffmpeg 和 ffprobe（brew install ffmpeg）。
# 在 Apple 芯片的 Mac 上，装了 Rosetta 的话再在 Rosetta 下把回声消除、自动增益的测试跑一遍 Intel 那一份（WebRTC 的 SSE2/AVX2 代码）；
# 不想跑设 ZSMEET_RECORDER_TEST_X86=0。
#
# 用法：desktop/recorder/test.sh（sh desktop/recorder/test.sh 也行）
#       只跑名字里有某几个字的测试：ZSMEET_RECORDER_TEST_FILTER=暂停 desktop/recorder/test.sh，几个用「|」隔开
#       （一个测试都没选中时算没通过，多半是写错了；设了它就不在 Rosetta 下再跑）
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
"$here/../scripts/build-recorder.sh"

# Homebrew 装的 ffmpeg 可能不在 PATH 里
export PATH="$PATH:/opt/homebrew/bin:/usr/local/bin"
for tool in ffmpeg ffprobe; do
  if ! command -v "$tool" >/dev/null; then
    echo "测试需要 ${tool}：brew install ffmpeg" >&2
    exit 1
  fi
done

sdk="$(xcrun --sdk macosx --show-sdk-path)"
include="$here/Sources/CRecorder/include"
# shellcheck source=ThirdParty/webrtc-flags.sh
source "$here/ThirdParty/webrtc-flags.sh"

# 编译一种架构的测试程序：.build/tests-<架构>/recorder-tests。WebRTC 用 build-recorder.sh 编好的静态库
build_tests() {
  local arch="$1"
  local target="$arch-apple-macos14.4"
  local out="$here/.build/tests-$arch"
  rm -rf "$out"
  mkdir -p "$out/modules"
  echo "编译测试（${arch}）…"
  xcrun clang -c -g -std=c11 -Wall -Wextra -Werror -target "$target" -isysroot "$sdk" \
    -I "$include" "$here/Sources/CRecorder/ring.c" -o "$out/ring.o"
  # 回声消除（zr_aec.cc）、自动增益（zr_agc.cc）的 C 接口：宏和 WebRTC 编译时一样，第三方的头文件当成系统头文件
  # （它们的警告不算）。不用 <(…)：sh desktop/recorder/test.sh 时 macOS 的 sh（POSIX 模式的 bash 3.2）不认它
  local flags=() flag flag_list shim
  flag_list="$(webrtc_flags "$arch" | sed 's/^-I$/-isystem/')"
  while IFS= read -r flag; do flags+=("$flag"); done <<<"$flag_list"
  for shim in zr_aec zr_agc; do
    xcrun clang++ -c -g -std=c++17 -Wall -Wextra -Werror -isysroot "$sdk" "${flags[@]}" \
      -I "$include" "$here/Sources/CRecorder/$shim.cc" -o "$out/$shim.o"
  done
  xcrun swiftc -Onone -g -enable-testing -swift-version 5 -target "$target" -sdk "$sdk" \
    -parse-as-library -module-name RecorderKit -emit-library -static \
    -emit-module -emit-module-path "$out/modules/RecorderKit.swiftmodule" \
    -I "$include" -Xcc -fmodule-map-file="$include/module.modulemap" \
    "$here"/Sources/RecorderKit/*.swift -o "$out/libRecorderKit.a"
  xcrun swiftc -Onone -g -swift-version 5 -target "$target" -sdk "$sdk" -module-name recorder_tests \
    -I "$out/modules" -I "$include" -Xcc -fmodule-map-file="$include/module.modulemap" \
    "$here"/Tests/recorder-tests/*.swift "$out/libRecorderKit.a" "$out/ring.o" "$out/zr_aec.o" "$out/zr_agc.o" \
    "$here/.build/webrtc-$arch/libwebrtc_apm.a" -lc++ -framework CoreFoundation \
    -o "$out/recorder-tests"
}

arch="$(uname -m)"
build_tests "$arch"
echo "运行测试…"
ZSMEET_RECORDER="$here/.build/release-universal/zsmeet-recorder" "$here/.build/tests-$arch/recorder-tests"

# Intel 那一份的 WebRTC（SSE2、AVX2 的代码）在 Apple 芯片上平时跑不到：有 Rosetta 的话在它下面跑一遍用到 WebRTC 的
# 测试（AEC3、AGC2 本身和回声消除、自动增益的端到端测试；EchoStage、MicGainStage 自己的逻辑和架构无关，不再跑一遍）
if [[ "$arch" == arm64 && "${ZSMEET_RECORDER_TEST_X86:-1}" != 0 && -z "${ZSMEET_RECORDER_TEST_FILTER:-}" ]]; then
  if arch -x86_64 /usr/bin/true 2>/dev/null; then
    build_tests x86_64
    echo "在 Rosetta 下运行 Intel 那一份的回声消除、自动增益测试…"
    ZSMEET_RECORDER_TEST_FILTER="（AEC3）|端到端：回声消除|（AGC2）|端到端：麦克风自动增益" ZSMEET_RECORDER="$here/.build/release-universal/zsmeet-recorder" \
      arch -x86_64 "$here/.build/tests-x86_64/recorder-tests"
  else
    echo "这台 Mac 没装 Rosetta，跳过 Intel 那一份的回声消除、自动增益测试"
  fi
fi
