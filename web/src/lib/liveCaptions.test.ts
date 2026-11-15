import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { groupCaptionLines, isNearBottom } from "./liveCaptions.ts";

describe("groupCaptionLines", () => {
  it("merges nearby finalized sentences while keeping provisional sentences separate", () => {
    const lines = groupCaptionLines(
      [
        { start: 0.2, end: 2.0, text: "大家早上好，" },
        { start: 2.4, end: 4.5, text: "今天过一下下周发版计划。" },
        { start: 7.0, end: 9.0, text: "First item is i18n." },
        { start: 9.3, end: 11.2, text: "Second item is live captions." },
        { start: 11.5, end: 12.8, text: "正在说的半句话" },
      ],
      4,
    );

    assert.equal(lines.length, 3);
    assert.deepEqual(lines[0], {
      start: 0.2,
      end: 4.5,
      text: "大家早上好，今天过一下下周发版计划。",
      provisional: false,
    });
    assert.deepEqual(lines[1], {
      start: 7.0,
      end: 11.2,
      text: "First item is i18n. Second item is live captions.",
      provisional: false,
    });
    assert.deepEqual(lines[2], {
      start: 11.5,
      end: 12.8,
      text: "正在说的半句话",
      provisional: true,
    });
  });

  it("handles Korean word spacing and clamps out-of-range finalizedCount", () => {
    const lines = groupCaptionLines(
      [
        { start: 0, end: 1.5, text: "안녕하세요." },
        { start: 1.8, end: 3.2, text: "회의를 시작하겠습니다." },
      ],
      99,
    );
    assert.equal(lines.length, 1);
    assert.equal(lines[0]?.text, "안녕하세요. 회의를 시작하겠습니다.");
    assert.equal(lines[0]?.provisional, false);
  });
});

describe("isNearBottom", () => {
  it("detects when scroll position is within threshold of bottom", () => {
    assert.equal(isNearBottom(264, 200, 500), true);
    assert.equal(isNearBottom(263, 200, 500), false);
    assert.equal(isNearBottom(0, 200, 180), true);
  });
});
