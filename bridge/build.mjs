import { build } from 'esbuild';
await build({
  entryPoints: ['src/main.ts'],
  bundle: true,
  platform: 'node',
  format: 'esm',
  target: 'node20',
  outfile: 'dist/protonfs-bridge.mjs',
  alias: { 'openpgp/lightweight': 'openpgp' },
  banner: { js: "import{createRequire as __cr}from'module';const require=__cr(import.meta.url);" },
  logLevel: 'warning',
  legalComments: 'external',
});
