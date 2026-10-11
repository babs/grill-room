import { execFileSync } from 'node:child_process';

// Playwright may kill the docker CLI without it stopping the container.
export default function teardown() {
  try {
    execFileSync('docker', ['rm', '-f', 'grill-room-e2e-mailpit'], { stdio: 'ignore' });
  } catch { /* already gone */ }
}
