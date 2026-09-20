// 将锁定依赖中的浏览器产物复制到 Go 的嵌入目录；不重新打包第三方源码。
// 修改依赖或首次构建时先执行依赖安装，再运行本脚本，最后构建 Go 服务。
import { copyFile, mkdir } from 'node:fs/promises';
const output = new URL('../internal/web/static/vendor/', import.meta.url);
await mkdir(output, { recursive: true });
await copyFile(new URL('../node_modules/@google/model-viewer/dist/model-viewer.min.js', import.meta.url), new URL('model-viewer.min.js', output));
// 分发查看器时一并保留上游许可证。
await copyFile(new URL('../node_modules/@google/model-viewer/LICENSE', import.meta.url), new URL('MODEL_VIEWER_LICENSE', output));
console.log('3D viewer 已准备好，可构建 Go 服务。');
