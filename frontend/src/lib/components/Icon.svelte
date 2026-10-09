<script lang="ts">
  let { name, size = 20 }: { name: string; size?: number } = $props();
  const paths: Record<string, string> = {
    grid: 'M3 3h7v7H3zM14 3h7v7h-7zM3 14h7v7H3zM14 14h7v7h-7z',
    server: 'M4 3h16v7H4zM4 14h16v7H4zM7 6.5h.01M7 17.5h.01M11 6.5h6M11 17.5h6',
    layers: 'm12 3 10 6-10 6L2 9zM2 13l10 6 10-6M2 17l10 6 10-6',
    book: 'M4 3h6a3 3 0 0 1 3 3v15a3 3 0 0 0-3-3H4zM13 6a3 3 0 0 1 3-3h5v15h-5a3 3 0 0 0-3 3',
    key: 'M15 3a6 6 0 1 1-4 10L3 21H0v-3l9-9a6 6 0 0 1 6-6zM16 7h.01',
    wallet: 'M20 6H4a2 2 0 0 1 0-4h14v4M2 4v16h20V6M16 11h6v5h-6z',
    arrow: 'M5 12h14M13 6l6 6-6 6',
    refresh: 'M20 7v5h-5M4 17v-5h5M5.5 8a7 7 0 0 1 12-3L20 8M4 16l2.5 3a7 7 0 0 0 12-3',
    link: 'm10 13 4-4M8 16l-1 1a4 4 0 0 1-6-6l4-4a4 4 0 0 1 6 0M16 8l1-1a4 4 0 0 1 6 6l-4 4a4 4 0 0 1-6 0',
    chip: 'M6 6h12v12H6zM9 9h6v6H9zM9 2v4M15 2v4M9 18v4M15 18v4M2 9h4M2 15h4M18 9h4M18 15h4',
    activity: 'M2 12h5l3-8 4 16 3-8h5',
    check: 'm5 12 4 4L19 6',
    close: 'm6 6 12 12M6 18 18 6',
    info: 'M12 8h.01M12 12v5M22 12a10 10 0 1 1-20 0 10 10 0 0 1 20 0',
    globe: 'M22 12a10 10 0 1 1-20 0 10 10 0 0 1 20 0M2 12h20M12 2a17 17 0 0 1 0 20 17 17 0 0 1 0-20',
    bolt: 'm13 2-9 12h7l-1 8 10-12h-7z',
    copy: 'M9 9h12v12H9zM15 9V3H3v12h6'
  };
</script>

<svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.65" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d={paths[name] || paths.grid} /></svg>
