import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { renderHomebrewCask, sha256File } from "./homebrew-cask.mjs";

const SAMPLE_SHA = "67abce4126ffa71d770a9a8c4ba33bd5d71439dd58d0ca5b380197dd3a7a3f59";

test("生成带版本号、SHA-256、去隔离标记与卸载清理路径的 Cask", () => {
  const rb = renderHomebrewCask({
    version: "0.1.0",
    sha256: SAMPLE_SHA.toUpperCase(),
  });
  assert.match(rb, /version "0\.1\.0"/);
  assert.match(rb, new RegExp(`sha256 "${SAMPLE_SHA}"`));
  assert.match(
    rb,
    /url "https:\/\/github\.com\/ZaneXo\/zsmeet\/releases\/download\/desktop-v#\{version\}\/zsmeet-desktop-#\{version\}-macos-universal\.dmg"/,
  );
  assert.match(rb, /depends_on macos: :sonoma/);
  assert.match(rb, /postflight_steps do/);
  assert.match(rb, /com\.apple\.quarantine/);
  assert.match(rb, /\{\{appdir\}\}\/zsmeet\.app/);
});

test("拒绝预发布版本号、非法 SHA-256 和非法仓库名", () => {
  assert.throws(() => renderHomebrewCask({ version: "0.2.0-rc.1", sha256: SAMPLE_SHA }), /正式版版本号/);
  assert.throws(() => renderHomebrewCask({ version: "v0.1.0", sha256: SAMPLE_SHA }), /正式版版本号/);
  assert.throws(() => renderHomebrewCask({ version: "0.1.0", sha256: "1234" }), /SHA-256/);
  assert.throws(() => renderHomebrewCask({ version: "0.1.0", sha256: SAMPLE_SHA, repo: "bad repo" }), /owner\/repo/);
});

test("计算本地 DMG 文件的 SHA-256", () => {
  const dir = mkdtempSync(join(tmpdir(), "zsmeet-cask-"));
  try {
    const file = join(dir, "sample.dmg");
    writeFileSync(file, "zsmeet-dmg-bytes");
    assert.equal(sha256File(file), "98328cb7f95e89a698abe05e353846c56bfcecb0b180a0b120f4c0ba78d82d92");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
