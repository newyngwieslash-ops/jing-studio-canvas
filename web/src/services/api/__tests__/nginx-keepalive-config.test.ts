import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

interface LocationBlock {
    selector: string;
    body: string;
}

function stripCommentLines(conf: string): string {
    return conf.replace(/\r\n?/g, "\n").replace(/^[ \t]*#.*$/gm, "");
}

function extractLocationBlocks(conf: string): LocationBlock[] {
    const cleaned = stripCommentLines(conf);
    const blocks: LocationBlock[] = [];
    const headerPattern = /^[ \t]*location[ \t]+(.+?)[ \t]*\{[ \t]*$/gm;

    for (const match of cleaned.matchAll(headerPattern)) {
        const selector = match[1].trim();
        const openBraceOffset = match[0].lastIndexOf("{");
        assert.ok(openBraceOffset >= 0, `Missing opening brace for selector: ${selector}`);
        const openBraceIndex = match.index + openBraceOffset;
        let depth = 1;
        let cursor = openBraceIndex + 1;
        while (cursor < cleaned.length && depth > 0) {
            const ch = cleaned[cursor];
            if (ch === "{") depth += 1;
            else if (ch === "}") depth -= 1;
            cursor += 1;
        }
        assert.equal(depth, 0, `Unclosed location block for selector: ${selector}`);
        blocks.push({
            selector,
            body: cleaned.slice(openBraceIndex + 1, cursor - 1),
        });
    }

    return blocks;
}

describe("nginx.conf upstream keepalive contract", () => {
    const nginxConfPath = fileURLToPath(new URL("../../../../../nginx.conf", import.meta.url));
    const rawConf = readFileSync(nginxConfPath, "utf8");
    const conf = stripCommentLines(rawConf);
    const serverStartIndex = conf.search(/^[ \t]*server[ \t]*\{/m);
    const topLevelBeforeServer = serverStartIndex >= 0 ? conf.slice(0, serverStartIndex) : "";
    const locationBlocks = extractLocationBlocks(rawConf);

    it("defines top-level map $http_upgrade $connection_upgrade preserving empty Connection on non-upgrade requests", () => {
        assert.ok(serverStartIndex > 0, "Expected top-level directives before server block");
        const mapMatch = topLevelBeforeServer.match(/map\s+\$http_upgrade\s+\$connection_upgrade\s*\{([^}]*)\}/);
        assert.ok(mapMatch, "Expected top-level `map $http_upgrade $connection_upgrade { ... }` block");
        const mapBody = mapMatch[1];
        assert.match(mapBody, /\bdefault\s+upgrade\s*;/);
        assert.match(mapBody, /(?:""|'')\s+(?:""|'')\s*;/);
        assert.doesNotMatch(mapBody, /\bclose\s*;/);
    });

    it("defines top-level upstream backend_upstream with server backend:8080 and keepalive 64", () => {
        assert.ok(serverStartIndex > 0, "Expected top-level directives before server block");
        const upstreamMatch = topLevelBeforeServer.match(/upstream\s+backend_upstream\s*\{([^}]*)\}/);
        assert.ok(upstreamMatch, "Expected top-level `upstream backend_upstream { ... }` block");
        const upstreamBody = upstreamMatch[1];
        assert.match(upstreamBody, /\bserver\s+backend:8080\s*;/);
        assert.match(upstreamBody, /\bkeepalive\s+64\s*;/);
    });

    it("routes every backend proxy_pass through http://backend_upstream with zero direct backend:8080 proxy_pass directives", () => {
        assert.doesNotMatch(conf, /proxy_pass\s+http:\/\/backend:8080\b/);
        const allProxyPasses = [...conf.matchAll(/\bproxy_pass\s+([^;]+);/g)].map((m) => m[1].trim());
        assert.equal(allProxyPasses.length, 8, "Expected all 8 backend proxy locations to remain present");
        for (const target of allProxyPasses) {
            assert.equal(target, "http://backend_upstream");
        }
    });

    it("sets HTTP/1.1 and empty Connection header on all 7 non-upgrade backend locations while keeping /api/ buffering policy intact", () => {
        const expectedHttpKeepaliveSelectors = [
            "= /api/admin/ecommerce/skills/install",
            "= /api/admin/ecommerce/skills/install/github",
            "^~ /api/public/canvas-shares/",
            "~ ^/api/(tasks/[^/]+/text-events|agent/runs/[^/]+/events)$",
            "~ ^/api/ai/system/[^/]+/(?:responses|chat/completions|models/[^/]+:streamGenerateContent)$",
            "/api/",
            "= /oauth/linuxdo/callback",
        ];

        const nonUpgradeProxyBlocks = locationBlocks.filter((block) => /\bproxy_pass\b/.test(block.body) && block.selector !== "~ ^/api/canvas-projects/[^/]+/realtime$");

        assert.deepEqual(
            nonUpgradeProxyBlocks.map((block) => block.selector),
            expectedHttpKeepaliveSelectors,
        );

        for (const block of nonUpgradeProxyBlocks) {
            assert.match(block.body, /\bproxy_pass\s+http:\/\/backend_upstream\s*;/, `Missing backend_upstream proxy_pass in location ${block.selector}`);
            assert.match(block.body, /\bproxy_http_version\s+1\.1\s*;/, `Missing proxy_http_version 1.1 in location ${block.selector}`);
            assert.match(block.body, /\bproxy_set_header\s+Connection\s+""\s*;/, `Missing empty Connection header in location ${block.selector}`);
        }

        const generalApiBlock = nonUpgradeProxyBlocks.find((block) => block.selector === "/api/");
        assert.ok(generalApiBlock, "Expected `location /api/` block");
        assert.match(generalApiBlock.body, /\bproxy_pass_header\s+X-Accel-Buffering\s*;/);
        assert.match(generalApiBlock.body, /\bproxy_read_timeout\s+3600s\s*;/);
        assert.doesNotMatch(generalApiBlock.body, /\bproxy_buffering\s+off\s*;/);
        assert.doesNotMatch(generalApiBlock.body, /\bproxy_cache\s+off\s*;/);
        assert.doesNotMatch(generalApiBlock.body, /\bgzip\s+off\s*;/);
    });

    it("preserves WebSocket upgrade compatibility via $connection_upgrade on the canvas realtime location", () => {
        const realtimeBlock = locationBlocks.find((block) => block.selector === "~ ^/api/canvas-projects/[^/]+/realtime$");
        assert.ok(realtimeBlock, "Expected canvas realtime location block");
        assert.match(realtimeBlock.body, /\bproxy_pass\s+http:\/\/backend_upstream\s*;/);
        assert.match(realtimeBlock.body, /\bproxy_http_version\s+1\.1\s*;/);
        assert.match(realtimeBlock.body, /\bproxy_set_header\s+Upgrade\s+\$http_upgrade\s*;/);
        assert.match(realtimeBlock.body, /\bproxy_set_header\s+Connection\s+\$connection_upgrade\s*;/);
        assert.doesNotMatch(realtimeBlock.body, /\bproxy_set_header\s+Connection\s+"upgrade"\s*;/);
    });

    it("parses location blocks with CRLF line endings, comments, and trailing whitespace after opening braces", () => {
        const snippet = [
            "server {",
            "    # leading comment",
            "    location /api/ {   \t ",
            "        # comment with { brace } inside",
            '        proxy_set_header Connection "";',
            "    }",
            "    location = /health {\t",
            "        return 200;",
            "    }",
            "}",
        ].join("\r\n");

        const parsed = extractLocationBlocks(snippet);
        assert.equal(parsed.length, 2);
        assert.equal(parsed[0].selector, "/api/");
        assert.doesNotMatch(parsed[0].body, /\r/);
        assert.doesNotMatch(parsed[0].body, /comment with/);
        assert.match(parsed[0].body, /proxy_set_header Connection "";/);
        assert.equal(parsed[1].selector, "= /health");
        assert.match(parsed[1].body, /return 200;/);
    });
});
