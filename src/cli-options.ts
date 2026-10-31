import { resolve } from 'node:path';

export interface CliOptions {
    command: 'help' | 'init' | 'doctor' | 'run';
    configPath: string;
    probe: boolean;
}

/** Parse before touching configuration, state or the network. */
export function parseCli(argv: string[], cwd = process.cwd()): CliOptions {
    const [first = 'help', ...rest] = argv;
    const command = first === '--help' || first === '-h' ? 'help' : first;
    if (!['help', 'init', 'doctor', 'run'].includes(command))
        throw new Error(`Unknown command ${JSON.stringify(first)}. Use --help.`);
    let config = 'config.json', probe = false, configSeen = false;
    for (let i = 0; i < rest.length; i++) {
        const arg = rest[i]!;
        if (arg === '--config' || arg.startsWith('--config=')) {
            if (configSeen) throw new Error('--config may only be specified once.');
            const value = arg === '--config' ? rest[++i] : arg.slice('--config='.length);
            if (!value?.trim() || value.startsWith('-') || value.includes('\0'))
                throw new Error('--config requires a file path. Example: --config ./config.json');
            config = value;
            configSeen = true;
        } else if (arg === '--probe' && command === 'doctor' && !probe) {
            probe = true;
        } else if (arg === '--help' || arg === '-h') {
            return { command: 'help', configPath: resolve(cwd, config), probe: false };
        } else {
            throw new Error(`Unexpected argument ${JSON.stringify(arg)}. --probe is supported only by doctor. Use --help.`);
        }
    }
    return { command: command as CliOptions['command'], configPath: resolve(cwd, config), probe };
}
