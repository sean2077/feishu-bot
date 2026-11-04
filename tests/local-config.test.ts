import test from 'node:test';
import assert from 'node:assert/strict';
import { resolve } from 'node:path';
import { parseConfig } from '../src/config.ts';

const base = () => ({ tenantId: 'tenant_fixture', appId: 'cli_fixture',
    model: { baseUrl: 'https://model.example', model: 'fixture-model' },
    projects: [{ id: 'assistant', maintainers: ['ou_fixture'] }] });

test('existing configuration keeps isolated native settings and acceptEdits defaults', () => {
    const config = parseConfig(base());
    assert.equal(config.projects[0]!.nativeConfig, 'isolated');
    assert.equal(config.projects[0]!.permissionMode, 'acceptEdits');
    assert.equal(config.projects[0]!.cwd, undefined);
    assert.equal(config.runtime.claudeExecutable, undefined);
});

test('direct cwd and optional Claude executable resolve relative to the configuration directory', () => {
    const config = parseConfig({ ...base(), runtime: { claudeExecutable: 'bin/claude.exe' },
        projects: [{ ...base().projects[0], cwd: 'working files', nativeConfig: 'inherit', permissionMode: 'bypassPermissions' }] }, resolve('fixture-config'));
    assert.equal(config.projects[0]!.cwd, resolve('fixture-config', 'working files'));
    assert.equal(config.runtime.claudeExecutable, resolve('fixture-config', 'bin/claude.exe'));
    assert.equal(config.projects[0]!.nativeConfig, 'inherit');
    assert.equal(config.projects[0]!.permissionMode, 'bypassPermissions');
    assert.equal(parseConfig({ ...base(), projects: [{ ...base().projects[0], nativeConfig: 'inherit' }] }).projects[0]!.cwd, undefined);
});

test('direct cwd rejects ambiguous copy and trusted configuration sources', () => {
    for (const field of ['seedDir', 'trustedConfigDir'])
        assert.throws(() => parseConfig({ ...base(), projects: [{ ...base().projects[0], cwd: 'work', [field]: 'source' }] }), /cannot be combined/);
});

test('new local configuration fields reject invalid values without reflecting their contents', () => {
    for (const [field, value] of [['nativeConfig', 'unknown-private-value'], ['cwd', ''], ['permissionMode', 'auto']] as const)
        assert.throws(() => parseConfig({ ...base(), projects: [{ ...base().projects[0], [field]: value }] }));
    for (const claudeExecutable of ['', 12, null])
        assert.throws(() => parseConfig({ ...base(), runtime: { claudeExecutable } }), /claudeExecutable/);
    assert.throws(() => parseConfig({ ...base(), runtime: { executable: 'claude' } }), /unknown field/);
});
