import { copyFile, mkdir } from 'node:fs/promises';
const output = new URL('../internal/web/static/vendor/', import.meta.url);
await mkdir(output, { recursive: true });
await copyFile(new URL('../node_modules/@google/model-viewer/dist/model-viewer.min.js', import.meta.url), new URL('model-viewer.min.js', output));
await copyFile(new URL('../node_modules/@google/model-viewer/LICENSE', import.meta.url), new URL('MODEL_VIEWER_LICENSE', output));
console.log('3D viewer 已准备好，可构建 Go 服务。');
