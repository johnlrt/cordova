const piconeroPerXMR = 1_000_000_000_000n;
const maxPiconero = 9_000_000_000_000_000_000n;

/** Convert a human-entered XMR cap to the server's exact integer unit. */
export function parseXmrCap(value: string): string | null {
  const input = value.trim();
  const match = /^(0|[1-9][0-9]{0,6})(?:\.([0-9]{1,12}))?$/.exec(input);
  if (!match) return null;
  const whole = BigInt(match[1]);
  const fraction = BigInt((match[2] ?? '').padEnd(12, '0') || '0');
  const piconero = whole * piconeroPerXMR + fraction;
  return piconero <= maxPiconero ? piconero.toString() : null;
}

/** Format canonical piconero without converting fractional units to a float. */
export function formatXmrCap(piconero: string, locale: string): string {
  if (!/^(0|[1-9][0-9]{0,18})$/.test(piconero)) return '—';
  const value = BigInt(piconero);
  if (value > maxPiconero) return '—';
  const whole = (value / piconeroPerXMR).toString();
  const groupedWhole = Number(whole).toLocaleString(locale, { maximumFractionDigits: 0 });
  const fraction = (value % piconeroPerXMR).toString().padStart(12, '0').replace(/0+$/, '');
  return fraction ? `${groupedWhole}.${fraction} XMR` : `${groupedWhole} XMR`;
}
