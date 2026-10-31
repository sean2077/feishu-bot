/**
 * Connect only while the service is open. Disconnect a connection that succeeds
 * after shutdown has already begun, instead of resurrecting a stopped listener.
 * The caller owns the initial disconnect during shutdown.
 */
export async function connectBeforeStop(
    channel: { connect(): Promise<unknown>; disconnect(): Promise<unknown> }, signal: AbortSignal
): Promise<boolean> {
    if (signal.aborted) return false;
    let onAbort: () => void = () => {};
    const stopped = new Promise<false>(resolve => {
        onAbort = () => resolve(false);
        signal.addEventListener('abort', onAbort, { once: true });
    });
    try {
        const connecting = channel.connect();
        const ready = await Promise.race([connecting.then(() => true), stopped]);
        if (!ready || signal.aborted) {
            // A late successful connect may reopen the SDK transport after its
            // first disconnect. Close that late connection without retrying it.
            void connecting.then(() => channel.disconnect()).catch(() => {});
            return false;
        }
        return true;
    } finally {
        signal.removeEventListener('abort', onAbort);
    }
}
