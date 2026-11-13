#!/usr/bin/env node
// 改桌面版的版本号：desktop/src-tauri/tauri.conf.json 的 version 和 Cargo.toml 里 [package] 的 version 一起改，
// 只动这两行，文件里别的内容（格式、注释）都不变。make desktop-version VERSION=0.2.0 用它，Cargo.lock 由 make 接着
// 用 cargo 更新。发布时推的标签要是 desktop-v<这个版本>，发布流程（.github/workflows/desktop.yml）会核对。
//
// 用法：node desktop/scripts/set-version.mjs <版本>    比如 0.2.0，预发布版本写成 0.3.0-rc.1
import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

// semver，不带「+」后面的构建信息
const SEMVER = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/;
const dir = fileURLToPath(new URL("../src-tauri/", import.meta.url));

/** 把 text 里 [start, end) 这一段中第一处 pattern 的第 2 组换成 version。pattern 要有 3 组：前面、版本号、后面。 */
function replaceVersion(text, pattern, start, end, file, version) {
  const match = pattern.exec(text.slice(start, end));
  if (!match) throw new Error(`${file} 里没找到版本号，请手动改`);
  const at = start + match.index;
  return {
    old: match[2],
    text: text.slice(0, at) + match[1] + version + match[3] + text.slice(at + match[0].length),
  };
}

function main() {
  const version = process.argv[2] ?? "";
  if (!SEMVER.test(version)) {
    throw new Error(`「${version}」不是版本号。用法：make desktop-version VERSION=0.2.0（预发布版本写成 0.3.0-rc.1）`);
  }

  // tauri.conf.json：最外层的 "version" 是第一个
  const conf = "tauri.conf.json";
  const confText = readFileSync(dir + conf, "utf8");
  const confResult = replaceVersion(confText, /("version"\s*:\s*")([^"]*)(")/, 0, confText.length, conf, version);
  if (JSON.parse(confResult.text).version !== version) throw new Error(`${conf} 改完以后读出来的版本不对，请手动改`);

  // Cargo.toml：只在 [package] 这一段里找（到下一个 [ 开头的行为止）
  const cargo = "Cargo.toml";
  const cargoText = readFileSync(dir + cargo, "utf8");
  const header = /^\[package\][ \t]*$/m.exec(cargoText);
  if (!header) throw new Error(`${cargo} 里没有 [package]，请手动改`);
  const start = header.index + header[0].length;
  const next = /^\[/m.exec(cargoText.slice(start));
  const end = next ? start + next.index : cargoText.length;
  const cargoResult = replaceVersion(cargoText, /^(version\s*=\s*")([^"]*)(")/m, start, end, cargo, version);

  writeFileSync(dir + conf, confResult.text);
  writeFileSync(dir + cargo, cargoResult.text);
  console.log(`桌面版的版本：${confResult.old} → ${version}（${conf}；Cargo.toml 原来是 ${cargoResult.old}）`);
}

try {
  main();
} catch (err) {
  console.error(`没改版本号：${err.message}`);
  process.exit(1);
}
