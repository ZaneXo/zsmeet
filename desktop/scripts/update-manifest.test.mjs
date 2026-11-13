// update-manifest.mjs 的测试：node --test desktop/scripts/update-manifest.test.mjs（make desktop-test 和 CI 都会跑）。
// 下面的签名是真的 Tauri CLI（2.12.4）签出来的：用一把只为这个测试生成、之后就删掉的密钥
// （tauri signer generate），对 PAYLOAD 执行 tauri signer sign --app-version 0.2.0。和发布用的密钥无关。
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { generateKeyPairSync, sign } from "node:crypto";
import { existsSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

import {
  buildManifest,
  decodePublicKey,
  PLATFORMS,
  verifyUpdateSignature,
} from "./update-manifest.mjs";

const SCRIPT = fileURLToPath(new URL("./update-manifest.mjs", import.meta.url));

const PAYLOAD = Buffer.from("zsmeet 更新包签名测试用的文件\n", "utf8");
const TEST_PUBKEY =
  "dW50cnVzdGVkIGNvbW1lbnQ6IG1pbmlzaWduIHB1YmxpYyBrZXk6IDdBRDE5OEI3NTgyQjA4MTIKUldRU0NDdFl0NWpSZXRvYVZtZy9oM3A1cHdnbWFkRDJVTWdueG0zSUp2ZU1nRG5TTmFBQlRuR3oK";
const TEST_SIGNATURE =
  "dW50cnVzdGVkIGNvbW1lbnQ6IHNpZ25hdHVyZSBmcm9tIHRhdXJpIHNlY3JldCBrZXkKUlVRU0NDdFl0NWpSZXVlek8vUTBNV0kxcWFMSDMzdTl0RHhQVTdIQ29WZ2lTdnhBYmZwdHVEWFBjdGNlWFNyUGJhbUJCc0JOYzVFdzNCbkxRYVl5WUR0UW02cjN0dDYwV3dRPQp0cnVzdGVkIGNvbW1lbnQ6IHRpbWVzdGFtcDoxNzk0NDgwMDE3CWZpbGU6cGF5bG9hZC50eHQJdmVyc2lvbjowLjIuMApONjdGNWJmb1ErKzRhbndRREdZNkowWllVRWIxUjRnYW10b29RNFQ1elFzdWY0M1pKY1lJOFB5WTlkQUE4bmRxUHY4N3phZHQrUDNENHJlTnIxZEhDUT09Cg==";
// 发布用的公钥（App 里的那把），只用来测「不是这把密钥签的」
const RELEASE_PUBKEY =
  "dW50cnVzdGVkIGNvbW1lbnQ6IG1pbmlzaWduIHB1YmxpYyBrZXk6IEExRDY0RDFDNDc5OTdFODAKUldTQWZwbEhIRTNXb1VzODRsSll0Szl0Nkp5ZFhPd2Ruc1hCa1liTzQzSGgyTW5FY2M0a3VncmcK";

const b64 = (text) => Buffer.from(text, "utf8").toString("base64");
const unb64 = (text) => Buffer.from(text, "base64").toString("utf8");

test("公钥的密钥 ID 和公钥文件里写的一样", () => {
  assert.equal(decodePublicKey(TEST_PUBKEY).keyId, "7AD198B7582B0812");
  assert.equal(decodePublicKey(RELEASE_PUBKEY).keyId, "A1D64D1C47997E80");
});

test("Tauri 签的更新包能校验通过，读得出签名里记的版本", () => {
  const result = verifyUpdateSignature(TEST_PUBKEY, PAYLOAD, TEST_SIGNATURE);
  assert.equal(result.keyId, "7AD198B7582B0812");
  assert.equal(result.signedVersion, "0.2.0");
  assert.match(result.trustedComment, /^timestamp:\d+\tfile:payload\.txt\tversion:0\.2\.0$/);
});

test("别的密钥签的：说清楚两把密钥的 ID", () => {
  assert.throws(
    () => verifyUpdateSignature(RELEASE_PUBKEY, PAYLOAD, TEST_SIGNATURE),
    /另一把密钥签的（密钥 ID 7AD198B7582B0812）.*A1D64D1C47997E80/,
  );
});

test("更新包签名以后被改过：不通过", () => {
  const changed = Buffer.from(PAYLOAD);
  changed[0] ^= 1;
  assert.throws(() => verifyUpdateSignature(TEST_PUBKEY, changed, TEST_SIGNATURE), /签名和更新包对不上/);
});

test("trusted comment 被改过（比如改了版本）：不通过", () => {
  const forged = b64(unb64(TEST_SIGNATURE).replace("version:0.2.0", "version:9.9.9"));
  assert.throws(() => verifyUpdateSignature(TEST_PUBKEY, PAYLOAD, forged), /trusted comment 被改过/);
});

test("不是签名的东西：说不是 minisign 的格式", () => {
  assert.throws(() => verifyUpdateSignature(TEST_PUBKEY, PAYLOAD, b64("hello\nworld\n")), /不是 minisign 的格式/);
  assert.throws(() => verifyUpdateSignature(TEST_PUBKEY, PAYLOAD, "不是 base64"), /不是 base64/);
});

test("老格式（不先算 BLAKE2b）的签名也认，和 App 里一样", () => {
  const { publicKey, privateKey } = generateKeyPairSync("ed25519");
  const raw = Buffer.from(publicKey.export({ format: "jwk" }).x, "base64url");
  const keyId = Buffer.from([1, 2, 3, 4, 5, 6, 7, 8]);
  const pubkey = b64(`untrusted comment: test\n${Buffer.concat([Buffer.from("Ed"), keyId, raw]).toString("base64")}\n`);
  const signature = sign(null, PAYLOAD, privateKey);
  const trusted = "timestamp:1\tfile:x";
  const global = sign(null, Buffer.concat([signature, Buffer.from(trusted)]), privateKey);
  const sigFile = (alg) =>
    b64(
      `untrusted comment: test\n${Buffer.concat([Buffer.from(alg), keyId, signature]).toString("base64")}\n` +
        `trusted comment: ${trusted}\n${global.toString("base64")}\n`,
    );

  const result = verifyUpdateSignature(pubkey, PAYLOAD, sigFile("Ed"));
  assert.equal(result.keyId, "0807060504030201");
  assert.equal(result.signedVersion, null);
  // 同一个签名标成新格式（ED：签的是 BLAKE2b-512）就对不上
  assert.throws(() => verifyUpdateSignature(pubkey, PAYLOAD, sigFile("ED")), /签名和更新包对不上/);
});

test("清单：三种架构都指向同一个更新包，格式是 Tauri v2 的静态清单", () => {
  const url = "https://github.com/ZaneXo/zsmeet/releases/download/desktop-v0.2.0/zsmeet-desktop-0.2.0-macos-universal.app.tar.gz";
  const manifest = buildManifest({
    version: "0.2.0",
    notes: "修好了一些问题",
    pubDate: "2026-11-12T10:00:00Z",
    url,
    signature: `${TEST_SIGNATURE}\n`,
  });
  assert.deepEqual(Object.keys(manifest), ["version", "notes", "pub_date", "platforms"]);
  assert.deepEqual(Object.keys(manifest.platforms), PLATFORMS);
  assert.deepEqual(PLATFORMS, ["darwin-aarch64", "darwin-x86_64", "darwin-universal"]);
  for (const name of PLATFORMS) {
    assert.deepEqual(manifest.platforms[name], { signature: TEST_SIGNATURE, url });
  }
  // App 严格按 RFC 3339 解析：只有日期的不行
  assert.throws(
    () => buildManifest({ version: "0.2.0", notes: "", pubDate: "2026-11-12", url, signature: "s" }),
    /RFC 3339/,
  );
  assert.throws(
    () => buildManifest({ version: "0.2", notes: "", pubDate: "2026-11-12T10:00:00Z", url, signature: "s" }),
    /不是 semver/,
  );
  assert.throws(
    () => buildManifest({ version: "0.2.0", notes: "", pubDate: "2026-11-12T10:00:00Z", url: "http://x/y", signature: "s" }),
    /https/,
  );
});

const ENDPOINT = "https://github.com/ZaneXo/zsmeet/releases/download/desktop-latest/latest.json";
const updaterConfig = (pubkey, more = {}) => ({ pubkey, endpoints: [ENDPOINT], ...more });

function runCli(config, extra = {}) {
  const dir = mkdtempSync(join(tmpdir(), "zsmeet-update-manifest-"));
  writeFileSync(join(dir, "tauri.conf.json"), JSON.stringify(config));
  writeFileSync(join(dir, "zsmeet.app.tar.gz"), extra.archive ?? PAYLOAD);
  writeFileSync(join(dir, "zsmeet.app.tar.gz.sig"), TEST_SIGNATURE);
  writeFileSync(join(dir, "notes.md"), "  这一版的更新说明\n\n第二行\n");
  const out = join(dir, "latest.json");
  try {
    const stdout = execFileSync(
      process.execPath,
      [
        SCRIPT,
        "--config", join(dir, "tauri.conf.json"),
        "--archive", join(dir, "zsmeet.app.tar.gz"),
        "--signature", join(dir, "zsmeet.app.tar.gz.sig"),
        "--url", "https://example.com/zsmeet.app.tar.gz",
        "--notes-file", join(dir, "notes.md"),
        "--out", out,
        "--endpoint", ENDPOINT,
      ],
      { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] },
    );
    return { ok: true, stdout, manifest: JSON.parse(readFileSync(out, "utf8")) };
  } catch (err) {
    return { ok: false, stderr: err.stderr, status: err.status, wroteOut: existsSync(out) };
  }
}

test("命令行：校验通过就生成 latest.json", () => {
  const result = runCli({ version: "0.2.0", plugins: { updater: updaterConfig(TEST_PUBKEY) } });
  assert.ok(result.ok, result.stderr);
  assert.match(result.stdout, /签名校验通过（公钥 ID 7AD198B7582B0812，签名里的版本 0\.2\.0）/);
  assert.equal(result.manifest.version, "0.2.0");
  assert.equal(result.manifest.notes, "这一版的更新说明\n\n第二行");
  assert.match(result.manifest.pub_date, /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$/);
  assert.equal(result.manifest.platforms["darwin-aarch64"].signature, TEST_SIGNATURE);
});

test("命令行：签名不对、版本不对、没有公钥、查更新的地址不对时以 1 退出，不生成清单", () => {
  const cases = [
    [{ version: "0.2.0", plugins: { updater: updaterConfig(RELEASE_PUBKEY) } }, {}, /另一把密钥/],
    [{ version: "0.3.0", plugins: { updater: updaterConfig(TEST_PUBKEY) } }, {}, /签名时记的版本是 0\.2\.0，清单里是 0\.3\.0/],
    [{ version: "0.2.0", plugins: {} }, {}, /没有 plugins\.updater\.pubkey/],
    [{ version: "0.2.0", plugins: { updater: updaterConfig(TEST_PUBKEY) } }, { archive: "x" }, /签名和更新包对不上/],
    [
      { version: "0.2.0", plugins: { updater: updaterConfig(TEST_PUBKEY, { endpoints: ["https://example.com/latest.json"] }) } },
      {},
      /plugins\.updater\.endpoints）里没有 https:\/\/github\.com\/ZaneXo\/zsmeet\/releases\/download\/desktop-latest\/latest\.json/,
    ],
  ];
  for (const [config, extra, message] of cases) {
    const result = runCli(config, extra);
    assert.equal(result.ok, false);
    assert.equal(result.status, 1);
    assert.equal(result.wroteOut, false);
    assert.match(result.stderr, /^生成更新清单失败：/);
    assert.match(result.stderr, message);
  }
});
