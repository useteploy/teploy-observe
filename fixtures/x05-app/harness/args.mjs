export function parsePhase(args) {
  let phase = 'lcp';
  let supplied = false;
  for (let i = 0; i < args.length; i++) {
    const arg = args[i];
    const value = arg === '--phase' ? args[++i] : arg.startsWith('--phase=') ? arg.slice(8) : undefined;
    if (supplied || !['lcp', 'session'].includes(value)) throw new Error('Expected --phase lcp or --phase session');
    phase = value;
    supplied = true;
  }
  return phase;
}
