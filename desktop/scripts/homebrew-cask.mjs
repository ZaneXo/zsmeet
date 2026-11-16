#!/usr/bin/env node
// 生成自建 Homebrew Tap（ZaneXo/homebrew-tap）里的 Casks/zsmeet.rb。
//
// 用法：
//   node desktop/scripts/homebrew-cask.mjs --version 0.1.0 --dmg dist/zsmeet-desktop-0.1.0-macos-universal.dmg --out dist/zsmeet.rb
//   node desktop/scripts/homebrew-cask.mjs --version 0.1.0 --sha256 <64位十六进制> [--repo ZaneXo/zsmeet] --out Casks/zsmeet.rb
import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { pathToFileURL } from "node:url";
import { parseArgs } from "node:util";

// Homebrew Tap 只发正式版（带「-」的预发布版本不改 Cask）
const STABLE_SEMVER = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/;
const SHA256_HEX = /^[0-9a-f]{64}$/i;
const REPO_SLUG = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;

export function sha256File(path) {
  return createHash("sha256").update(readFileSync(path)).digest("hex");
}

export function renderHomebrewCask({ version, sha256, repo = "ZaneXo/zsmeet" }) {
  if (!STABLE_SEMVER.test(version ?? "")) {
    throw new Error(`「${version ?? ""}」不是正式版版本号（如 0.1.0，不含预发布后缀）`);
  }
  if (!SHA256_HEX.test(sha256 ?? "")) {
    throw new Error(`「${sha256 ?? ""}」不是 64 位十六进制 SHA-256`);
  }
  if (!REPO_SLUG.test(repo ?? "")) {
    throw new Error(`「${repo ?? ""}」不是 owner/repo 格式的仓库名`);
  }
  const lowerSha = sha256.toLowerCase();
  return `cask "zsmeet" do
  version "${version}"
  sha256 "${lowerSha}"

  url "https://github.com/${repo}/releases/download/desktop-v#{version}/zsmeet-desktop-#{version}-macos-universal.dmg"
  name "zsmeet"
  desc "Self-hosted meeting transcription and minutes desktop client"
  homepage "https://github.com/${repo}"

  livecheck do
    url "https://github.com/${repo}/releases/download/desktop-latest/latest.json"
    strategy :json do |json|
      json["version"]
    end
  end

  auto_updates true
  depends_on macos: (MacOSRequirement.respond_to?(:parse) ? :sonoma : ">= :sonoma")

  app "zsmeet.app"

  if respond_to?(:postflight_steps)
    postflight_steps do
      run "xattr", args: ["-dr", "com.apple.quarantine", "{{appdir}}/zsmeet.app"]
    end
  else
    postflight do
      system_command "/usr/bin/xattr", args: ["-dr", "com.apple.quarantine", "#{appdir}/zsmeet.app"]
    end
  end

  uninstall quit: "io.github.zanexo.zsmeet"

  zap trash: [
    "~/Library/Application Support/io.github.zanexo.zsmeet",
    "~/Library/Caches/io.github.zanexo.zsmeet",
    "~/Library/Preferences/io.github.zanexo.zsmeet.plist",
    "~/Library/Saved Application State/io.github.zanexo.zsmeet.savedState",
    "~/Library/WebKit/io.github.zanexo.zsmeet",
  ]
end
`;
}

function main() {
  const { values } = parseArgs({
    options: {
      version: { type: "string" },
      dmg: { type: "string" },
      sha256: { type: "string" },
      repo: { type: "string" },
      out: { type: "string" },
    },
  });
  if (!values.version || !values.out || (!values.dmg && !values.sha256) || (values.dmg && values.sha256)) {
    throw new Error("参数不全：需要 --version、--out，以及 --dmg 或 --sha256 二选一");
  }
  const sha256 = values.dmg ? sha256File(values.dmg) : values.sha256;
  const content = renderHomebrewCask({
    version: values.version,
    sha256,
    repo: values.repo || "ZaneXo/zsmeet",
  });
  writeFileSync(values.out, content);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    main();
  } catch (err) {
    console.error(`没能生成 Homebrew Cask：${err.message}`);
    process.exit(1);
  }
}
