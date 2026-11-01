import { spawnSync } from 'node:child_process';

// Windows hosted runners occasionally spend most of the existing 15-second
// PowerShell/CIM cleanup deadline starting PowerShell under file-test contention.
// Keep every test and assertion; serialize files only on Windows.
const concurrency = process.platform === 'win32' ? 1 : 2;
const result = spawnSync(process.execPath, [
    '--experimental-strip-types', '--test', `--test-concurrency=${concurrency}`, 'tests/*.test.ts'
], { stdio: 'inherit', windowsHide: true });

if (result.error) throw result.error;
if (result.signal) console.error(`Core tests were interrupted by ${result.signal}`);
process.exitCode = result.status ?? 1;
