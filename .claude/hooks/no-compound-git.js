#!/usr/bin/env node
const fs = require("fs");
const input = fs.readFileSync(0, "utf8");
const data = JSON.parse(input);
const command = data.tool_input?.command || "";

if (/\bmerge\b/.test(command) || /(&&|\|\||;)/.test(command)) {
  console.log(JSON.stringify({
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: "deny",
      permissionDecisionReason:
        "Do not use compound commands (e.g., pipes, `&&`, `;`, subshells). Run each command as a separate Bash invocation."
    }
  }));
}
process.exit(0);