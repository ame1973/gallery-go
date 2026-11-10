import { execFileSync } from 'node:child_process';
import {
  copyFileSync,
  mkdtempSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// Exercise the real Vite/Tailwind build in a newly created, disposable project.
describe('frontend build source isolation', () => {
  it('scans registered src and HTML, but not neighboring local files', () => {
    const directory = mkdtempSync(join(tmpdir(), 'gallerygo-css-test-'));
    const classFor = (color: string) => `bg-[${'#'}${color}]`;
    try {
      mkdirSync(join(directory, 'src', 'styles'), { recursive: true });
      copyFileSync(resolve('src/styles/app.css'), join(directory, 'src/styles/app.css'));
      symlinkSync(
        resolve('node_modules'),
        join(directory, 'node_modules'),
        process.platform === 'win32' ? 'junction' : 'dir',
      );
      writeFileSync(
        join(directory, 'vite.config.mjs'),
        "import tailwindcss from '@tailwindcss/vite'; export default { plugins: [tailwindcss()] };\n",
      );
      writeFileSync(
        join(directory, 'index.html'),
        `<div class="${classFor('d1e2f3')}"></div><script type="module" src="/src/main.ts"></script>`,
      );
      writeFileSync(join(directory, 'src/main.ts'), "import './styles/app.css';\n");
      writeFileSync(
        join(directory, 'src/component.tsx'),
        `<div className="${classFor('c1d2e3')}" />\n`,
      );
      writeFileSync(join(directory, '.gitignore'), '.env\n');
      writeFileSync(join(directory, 'src/.env'), `${classFor('b1c2d3')}\n`);
      writeFileSync(join(directory, 'private.txt'), `${classFor('a1b2c3')}\n`);
      execFileSync(process.execPath, [resolve('node_modules/vite/bin/vite.js'), 'build'], {
        cwd: directory,
        timeout: 30_000,
        stdio: 'pipe',
      });
      const assets = join(directory, 'dist', 'assets');
      const css = readdirSync(assets)
        .filter((file) => file.endsWith('.css'))
        .map((file) => readFileSync(join(assets, file), 'utf8'))
        .join('\n');
      expect(css).toContain('#d1e2f3');
      expect(css).toContain('#c1d2e3');
      expect(css).not.toContain('#a1b2c3');
      expect(css).not.toContain('#b1c2d3');
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  });
});
