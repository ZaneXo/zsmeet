// set-version.mjs 的测试：node --test desktop/scripts/set-version.test.mjs（make desktop-test 和 CI 都会跑）。
// 脚本改的是它旁边的 ../src-tauri/ 里的文件，所以把它复制到一个临时目录里，配上假的 tauri.conf.json 和 Cargo.toml 再跑。
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { copyFileSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const SCRIPT = fileURLToPath(new URL("./set-version.mjs", import.meta.url));

const CONF = `{
  "$schema": "https://schema.tauri.app/config/2",
  "productName": "zsmeet",
  "version": "0.1.0",
  "bundle": { "targets": ["app", "dmg"] },
  "plugins": { "x": { "version": "9.9.9" } }
}
`;

const CARGO = `# 注释里的 version = "1.0.0" 不算
[package]
name = "zsmeet-desktop"
version = "0.1.0"
edition = "2021"

[dependencies]
tauri = { version = "2", features = [] }

[dependencies.serde]
version = "1"
`;

function setup() {
  const root = mkdtempSync(join(tmpdir(), "zsmeet-set-version-"));
  mkdirSync(join(root, "desktop", "scripts"), { recursive: true });
  mkdirSync(join(root, "desktop", "src-tauri"));
  copyFileSync(SCRIPT, join(root, "desktop", "scripts", "set-version.mjs"));
  writeFileSync(join(root, "desktop", "src-tauri", "tauri.conf.json"), CONF);
  writeFileSync(join(root, "desktop", "src-tauri", "Cargo.toml"), CARGO);
  const run = (version) => {
    try {
      const stdout = execFileSync(process.execPath, [join(root, "desktop", "scripts", "set-version.mjs"), version], {
        encoding: "utf8",
        stdio: ["ignore", "pipe", "pipe"],
      });
      return { ok: true, stdout };
    } catch (err) {
      return { ok: false, status: err.status, stderr: err.stderr };
    }
  };
  const read = (name) => readFileSync(join(root, "desktop", "src-tauri", name), "utf8");
  return { run, read };
}

test("只改 tauri.conf.json 最外层的 version 和 Cargo.toml 里 [package] 的 version，别的都不动", () => {
  const { run, read } = setup();
  const result = run("0.2.0");
  assert.ok(result.ok, result.stderr);
  assert.match(result.stdout, /0\.1\.0 → 0\.2\.0/);
  assert.equal(read("tauri.conf.json"), CONF.replace('"version": "0.1.0"', '"version": "0.2.0"'));
  assert.equal(read("Cargo.toml"), CARGO.replace('\nversion = "0.1.0"', '\nversion = "0.2.0"'));

  assert.ok(run("0.3.0-rc.1").ok);
  assert.equal(JSON.parse(read("tauri.conf.json")).version, "0.3.0-rc.1");
  assert.match(read("Cargo.toml"), /^\[package\]\nname = "zsmeet-desktop"\nversion = "0\.3\.0-rc\.1"\n/m);
});

test("不是版本号时以 1 退出，两个文件都不改", () => {
  const { run, read } = setup();
  for (const bad of ["", "0.2", "v0.2.0", "0.2.0+1", "01.2.0", "0.2.0-"]) {
    const result = run(bad);
    assert.equal(result.ok, false, bad);
    assert.equal(result.status, 1);
    assert.match(result.stderr, /^没改版本号：.*不是版本号/);
  }
  assert.equal(read("tauri.conf.json"), CONF);
  assert.equal(read("Cargo.toml"), CARGO);
});
