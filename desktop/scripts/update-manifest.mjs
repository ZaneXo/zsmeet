#!/usr/bin/env node
// 生成桌面版自动更新用的清单 latest.json（Tauri v2 updater 的静态清单格式），生成之前先用 App 里的公钥
// （tauri.conf.json 的 plugins.updater.pubkey）校验更新包的签名。发布流程（.github/workflows/desktop.yml）用它：
// 签名不是这把公钥签的（Secrets 里的 TAURI_SIGNING_PRIVATE_KEY 配错了）、签名里记的版本和清单的版本对不上、
// App 查更新的地址里没有发布用的那个时，以 1 退出并说明原因，不生成清单。不然发出去以后，用户要么点了「更新」
// 因为签名校验失败装不上，要么根本收不到更新。
//
// 用法：
//   node desktop/scripts/update-manifest.mjs --config desktop/src-tauri/tauri.conf.json \
//     --archive <zsmeet.app.tar.gz> --signature <zsmeet.app.tar.gz.sig> \
//     --url <更新包的下载地址> --notes-file <更新说明> --out <latest.json> \
//     [--endpoint <清单发布到的地址>]    给了的话，App 查更新的地址（plugins.updater.endpoints）里要有它
//
// 清单里 darwin-aarch64、darwin-x86_64、darwin-universal 三项都指向同一个通用（universal）的更新包：
// App 按自己运行的架构找 darwin-<架构>（先找 darwin-<架构>-app），Apple 芯片和 Intel 都能找到。
// 签名是 minisign 格式（Tauri 用的就是它），这里照 App 里校验用的 minisign-verify 的规则校验，只用 Node 自带的 crypto。
import { createHash, createPublicKey, verify } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { pathToFileURL } from "node:url";
import { parseArgs } from "node:util";

export const PLATFORMS = ["darwin-aarch64", "darwin-x86_64", "darwin-universal"];

const SEMVER =
  /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/;

/** minisign 的密钥 ID：8 个字节按小端读成一个数，写成 16 位大写十六进制（和公钥文件第一行里写的一样）。 */
function keyIdHex(bytes) {
  return Buffer.from(bytes).reverse().toString("hex").toUpperCase();
}

/** 严格的 base64（一行）：不是的返回 null，免得 Buffer.from 悄悄跳过不认识的字符。 */
function base64(text) {
  const value = text.trim();
  return /^[A-Za-z0-9+/]+={0,2}$/.test(value) ? Buffer.from(value, "base64") : null;
}

/** 外面一层 base64 解开以后按行分，去掉行尾的 \r（和 App 里用的 Rust 的 lines() 一样）。 */
function base64Lines(text, what) {
  const outer = base64(text);
  if (outer === null) throw new Error(`${what}不是 base64`);
  return outer.toString("utf8").split("\n").map((line) => line.replace(/\r$/, ""));
}

/** tauri.conf.json 里的 pubkey：minisign 公钥文件（两行）整个再 base64 一次。 */
export function decodePublicKey(pubkeyBase64) {
  const raw = base64(base64Lines(pubkeyBase64, "公钥")[1] ?? "");
  if (raw === null || raw.length !== 42 || raw[0] !== 0x45 || (raw[1] !== 0x64 && raw[1] !== 0x44)) {
    throw new Error("公钥不是 minisign 的 Ed25519 公钥");
  }
  return { keyId: keyIdHex(raw.subarray(2, 10)), key: raw.subarray(10, 42) };
}

/** Tauri 生成的 .sig：minisign 签名文件（四行）整个再 base64 一次。 */
export function decodeSignature(signatureBase64) {
  const lines = base64Lines(signatureBase64, "签名");
  const sig = base64(lines[1] ?? "");
  const trusted = lines[2] ?? "";
  const global = base64(lines[3] ?? "");
  if (sig?.length !== 74 || global?.length !== 64 || !trusted.startsWith("trusted comment: ")) {
    throw new Error("签名不是 minisign 的格式（应该是 Tauri 生成的 .sig 文件的内容）");
  }
  let prehashed;
  if (sig[0] === 0x45 && sig[1] === 0x44) prehashed = true;
  else if (sig[0] === 0x45 && sig[1] === 0x64) prehashed = false;
  else throw new Error("签名用的算法不认识");
  return {
    prehashed,
    keyId: keyIdHex(sig.subarray(2, 10)),
    signature: sig.subarray(10, 74),
    trustedComment: trusted.slice("trusted comment: ".length),
    globalSignature: global,
  };
}

function ed25519Verify(key, message, signature) {
  const publicKey = createPublicKey({
    key: { kty: "OKP", crv: "Ed25519", x: Buffer.from(key).toString("base64url") },
    format: "jwk",
  });
  return verify(null, message, publicKey, signature);
}

/**
 * 校验 data 的签名，规则和 App 里一样：密钥 ID 对得上，签名对得上（新格式先算 BLAKE2b-512），
 * 「trusted comment」也是这把密钥签的。通过时返回 trusted comment 和里面记的版本（Tauri 签名时写进去的 version:…）。
 */
export function verifyUpdateSignature(pubkeyBase64, data, signatureBase64) {
  const publicKey = decodePublicKey(pubkeyBase64);
  const sig = decodeSignature(signatureBase64);
  if (sig.keyId !== publicKey.keyId) {
    throw new Error(
      `更新包是用另一把密钥签的（密钥 ID ${sig.keyId}），App 里的公钥是 ${publicKey.keyId}。` +
        "Secrets 里的 TAURI_SIGNING_PRIVATE_KEY 要换成和这个公钥配对的私钥",
    );
  }
  const message = sig.prehashed ? createHash("blake2b512").update(data).digest() : data;
  if (!ed25519Verify(publicKey.key, message, sig.signature)) {
    throw new Error("签名和更新包对不上：更新包在签名以后被改过，或者签名文件不是它的");
  }
  const global = Buffer.concat([sig.signature, Buffer.from(sig.trustedComment, "utf8")]);
  if (!ed25519Verify(publicKey.key, global, sig.globalSignature)) {
    throw new Error("签名里的 trusted comment 被改过");
  }
  const signedVersion =
    sig.trustedComment
      .split("\t")
      .find((field) => field.startsWith("version:"))
      ?.slice("version:".length) ?? null;
  return { keyId: publicKey.keyId, trustedComment: sig.trustedComment, signedVersion };
}

/** Tauri v2 updater 的静态清单。 */
export function buildManifest({ version, notes, pubDate, url, signature }) {
  if (!SEMVER.test(version)) throw new Error(`版本号 ${version} 不是 semver（比如 0.2.0）`);
  if (!/^https:\/\//.test(url)) throw new Error(`下载地址要用 https：${url}`);
  // App 按 RFC 3339 严格解析，解析不了整个清单都不认
  if (!/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)$/.test(pubDate)) {
    throw new Error(`发布时间 ${pubDate} 不是 RFC 3339（比如 2026-11-12T10:00:00Z）`);
  }
  const platform = { signature: signature.trim(), url };
  return {
    version,
    notes,
    pub_date: pubDate,
    platforms: Object.fromEntries(PLATFORMS.map((name) => [name, { ...platform }])),
  };
}

function main() {
  const { values } = parseArgs({
    options: {
      config: { type: "string" },
      archive: { type: "string" },
      signature: { type: "string" },
      url: { type: "string" },
      "notes-file": { type: "string" },
      out: { type: "string" },
      endpoint: { type: "string" },
    },
  });
  for (const name of ["config", "archive", "signature", "url", "notes-file", "out"]) {
    if (!values[name]) throw new Error(`缺少 --${name}（用法见文件开头）`);
  }

  const config = JSON.parse(readFileSync(values.config, "utf8"));
  const version = config.version;
  const updater = config.plugins?.updater;
  if (typeof updater?.pubkey !== "string" || updater.pubkey.trim() === "") {
    throw new Error(`${values.config} 里没有 plugins.updater.pubkey，App 没法校验更新包`);
  }
  // GitHub 的用户名、仓库名不分大小写（ZaneXo/zsmeet 和 zanexo/zsmeet 是同一个地址），其余部分本来就是小写的
  const endpoints = (updater.endpoints ?? []).map((url) => String(url).toLowerCase());
  if (values.endpoint && !endpoints.includes(values.endpoint.toLowerCase())) {
    throw new Error(
      `App 查更新的地址（${values.config} 的 plugins.updater.endpoints）里没有 ${values.endpoint}，` +
        "发到那里的更新 App 收不到",
    );
  }

  const signatureText = readFileSync(values.signature, "utf8").trim();
  const result = verifyUpdateSignature(updater.pubkey, readFileSync(values.archive), signatureText);
  // App 下载更新时也会核对：签名里记了版本的，要和清单里的版本一样；开了 requireSignedVersion 的，签名里必须记着版本
  if (result.signedVersion !== null && result.signedVersion.replace(/^v/, "") !== version) {
    throw new Error(`更新包签名时记的版本是 ${result.signedVersion}，清单里是 ${version}，App 会拒绝安装`);
  }
  if (result.signedVersion === null && updater.requireSignedVersion === true) {
    throw new Error("签名里没有记版本，而 App 开了 requireSignedVersion，会拒绝安装。请用 tauri build 重新签名");
  }

  const notes = readFileSync(values["notes-file"], "utf8").trim();
  const manifest = buildManifest({
    version,
    notes,
    pubDate: new Date().toISOString().replace(/\.\d+Z$/, "Z"),
    url: values.url,
    signature: signatureText,
  });
  writeFileSync(values.out, `${JSON.stringify(manifest, null, 2)}\n`);
  console.log(
    `签名校验通过（公钥 ID ${result.keyId}，签名里的版本 ${result.signedVersion ?? "没有记"}），` +
      `已生成 ${values.out}：版本 ${version}`,
  );
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  try {
    main();
  } catch (err) {
    console.error(`生成更新清单失败：${err.message}`);
    process.exit(1);
  }
}
