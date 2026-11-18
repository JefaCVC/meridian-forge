// Meridian Forge — entrypoint.
//
// Thin Node 22 (stdlib-only) launcher: it spawns the vendored statically
// linked Go binary (forge-linux-amd64), forwards env + stdio, and exits with
// the binary's code. All forge logic lives in the Go binary.
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const binary = join(here, "forge-linux-amd64");

const child = spawn(binary, process.argv.slice(2), {
  env: process.env,
  stdio: "inherit",
});

child.on("error", (err) => {
  console.error(`meridian-forge: failed to start ${binary}: ${err.message}`);
  process.exit(1);
});

child.on("exit", (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});

// Forward termination signals so the binary shuts down cleanly.
for (const sig of ["SIGINT", "SIGTERM"] as const) {
  process.on(sig, () => child.kill(sig));
}
