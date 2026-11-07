import assert from "node:assert/strict";
import { test } from "node:test";

import { averageShotSeconds, clampEpisodeSeconds, formatEpisodeSeconds, lineTarget, normalizeStoryboard, parseEpisodeSeconds, segmentLimits, segmentTargetOf, segmentTargetSeconds, shotTarget, withSegmentTarget } from "./storyboard";

test("parses free-form episode durations", () => {
    assert.equal(parseEpisodeSeconds("90"), 90);
    assert.equal(parseEpisodeSeconds("5分钟"), 300);
    assert.equal(parseEpisodeSeconds("5 min"), 300);
    assert.equal(parseEpisodeSeconds("2:30"), 150);
    assert.equal(parseEpisodeSeconds("1分30秒"), 90);
    assert.equal(parseEpisodeSeconds("1.5分钟"), 90);
    assert.equal(parseEpisodeSeconds("很长"), null);
    assert.equal(clampEpisodeSeconds(5000), 1200);
    assert.equal(formatEpisodeSeconds(300), "5 分钟");
    assert.equal(formatEpisodeSeconds(150), "2 分 30 秒");
});

test("storyboard rules mirror the backend", () => {
    const fast = normalizeStoryboard({ pacing: "fast", dialogue: "dense", segmentMode: "fixed", segmentSeconds: 10 });
    assert.equal(averageShotSeconds(fast), 2.5);
    assert.equal(shotTarget(fast, 300), 120);
    assert.equal(lineTarget(fast, 300), 100);
    assert.deepEqual(segmentLimits(fast, 15), { mode: "fixed", min: 10, max: 10 });
    assert.deepEqual(segmentLimits(normalizeStoryboard({ segmentMode: "fixed", segmentSeconds: 30 }), 15), { mode: "fixed", min: 15, max: 15 });
    assert.deepEqual(segmentLimits(normalizeStoryboard({}), 15), { mode: "auto", min: 0, max: 15 });
    const range = normalizeStoryboard({ segmentMode: "range", segmentMinSeconds: 20, segmentMaxSeconds: 8 });
    assert.deepEqual([range.segmentMinSeconds, range.segmentMaxSeconds], [8, 20]);
    assert.deepEqual(segmentLimits(range, 15), { mode: "range", min: 8, max: 15 });
    assert.equal(segmentTargetSeconds(segmentLimits(range, 15)), 11);
    assert.equal(normalizeStoryboard({ segmentSeconds: 2 }).segmentSeconds, 4);
    assert.equal(normalizeStoryboard({ segmentSeconds: 600 }).segmentSeconds, 60);
});

test("segment target shortcut writes the same storyboard fields", () => {
    assert.equal(segmentTargetOf(undefined), "auto");
    const fifteen = withSegmentTarget(undefined, 15);
    assert.deepEqual([fifteen.segmentMode, fifteen.segmentMinSeconds, fifteen.segmentMaxSeconds], ["range", 8, 15]);
    assert.equal(segmentTargetOf(fifteen), 15);
    // 最短 20 选 15 秒：夹成 15–15，不能被对调成 15–20。
    const clamped = withSegmentTarget({ segmentMode: "range", segmentMinSeconds: 20, segmentMaxSeconds: 30 }, 15);
    assert.deepEqual([clamped.segmentMinSeconds, clamped.segmentMaxSeconds], [15, 15]);
    const thirty = withSegmentTarget(fifteen, 30);
    assert.deepEqual([thirty.segmentMinSeconds, thirty.segmentMaxSeconds], [8, 30]);
    // 切回智能只改模式，数值保留。
    const auto = withSegmentTarget(thirty, "auto");
    assert.deepEqual([auto.segmentMode, auto.segmentMaxSeconds], ["auto", 30]);
    assert.equal(segmentTargetOf({ segmentMode: "fixed", segmentSeconds: 15 }), "custom");
    assert.equal(segmentTargetOf({ segmentMode: "range", segmentMinSeconds: 8, segmentMaxSeconds: 20 }), "custom");
});
