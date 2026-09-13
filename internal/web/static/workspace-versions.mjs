import {$,node,checks} from './workspace-dom.mjs';
import {fileURL,downloadURL} from './workspace-api.mjs';
import {sortedVersions} from './workspace-state.mjs';

export const versionName=version=>version?`v${version.version_number}`:'未找到的版本';
const operations={generate:'文本生成',regenerate:'参考描述重新生成',decimate:'模型减面'};
export function versionOrigin(version,versions) {
  if(version.provenance_status==='historical_unknown')return '历史来源无法完整核验';
  if(version.parent_version_id)return `由 ${versionName(versions.get(version.parent_version_id))} 加工 · ${operations[version.operation_kind]||'模型加工'}`;
  if(version.context_reference_id)return `参考 ${versionName(versions.get(version.context_reference_id))} 的描述重新生成，未沿用旧几何`;
  return operations[version.operation_kind]||'历史制作结果';
}
export function technicalSummary(report) {
  if(!report)return '尚无技术报告';
  if(!report.valid)return '文件无法处理 · 面数无法验证';
  const triangles=Number(report.triangles);
  const bytes=Number(report.bytes);
  return `${Number.isFinite(triangles)?triangles.toLocaleString('zh-CN'):'无法验证'} 三角面 · ${Number.isFinite(bytes)?(bytes/1048576).toFixed(2):'无法验证'} MiB`;
}
export function versionStatus(version) {
  if(version.processable!==true)return '当前文件不可处理';
  return version.report?.passed?'通过来源执行的技术检查':'候选 · 未通过来源执行的技术检查';
}

export class VersionsView {
  constructor({onPreview,onReference}) {Object.assign(this,{onPreview,onReference});this.lastList='';this.lastPreview='';}
  render(state) {
    const versions=sortedVersions(state),version=state.versions.get(state.previewID);
    $('version-count').textContent=versions.length;$('versions-total').textContent=versions.length;
    $('no-versions').hidden=!!versions.length;
    const signature=JSON.stringify([state.id,state.previewID,state.unavailable,versions]);
    if(signature!==this.lastList) {
      this.lastList=signature;
      $('versions').replaceChildren(...versions.slice().reverse().map(item=>this.card(state,item)));
    }
    const previewSignature=JSON.stringify([state.id,state.unavailable,version]);
    if(previewSignature===this.lastPreview)return;
    this.lastPreview=previewSignature;
    const valid=!!version?.report?.valid && version.processable===true && !state.unavailable;
    $('preview-title').textContent=version?`${versionName(version)} · 模型预览`:'模型预览';
    $('preview-info').hidden=!version;
    $('download').hidden=!version || state.unavailable;
    $('viewer').hidden=!valid;$('empty-model').hidden=valid;
    const empty=$('empty-model');
    empty.querySelector('strong').textContent=version?'这个版本当前无法预览':'模型将在这里成形';
    empty.querySelector('span:last-child').textContent=version?'请查看技术报告和文件可用情况。':'制作时可以继续查看已有版本。';
    if(!version||!valid)$('viewer').removeAttribute('src');
    if(!version){checks($('report'));return;}
    const src=fileURL(version,state.id);
    // 相同文件不重置查看器，用户旋转/缩放和相机位置不随进度更新丢失。
    if(valid && $('viewer').getAttribute('src')!==src)$('viewer').setAttribute('src',src);
    $('download').href=downloadURL(version,state.id);
    $('preview-status').textContent=versionStatus(version);
    $('preview-origin').textContent=versionOrigin(version,state.versions)+' · '+technicalSummary(version.report);
    $('reference-preview').disabled=version.processable!==true||state.unavailable;
    $('reference-preview').onclick=()=>this.onReference(version.id);
    checks($('report'),version.report?.checks||[]);
  }
  card(state,version) {
    const card=node('article',undefined,'version-card'+(state.previewID===version.id?' selected':''));card.dataset.versionId=version.id;
    const preview=node('button',versionName(version),'version-number');preview.type='button';preview.title='预览 '+versionName(version);preview.setAttribute('aria-label','预览 '+versionName(version));preview.onclick=()=>this.onPreview(version.id);
    const info=node('div',undefined,'version-info');info.append(node('strong',versionStatus(version)),node('p',technicalSummary(version.report)),node('p',versionOrigin(version,state.versions)));
    const actions=node('div',undefined,'version-actions');
    const reference=node('button','引用');reference.type='button';reference.setAttribute('aria-label','引用 '+versionName(version)+' 到聊天');reference.disabled=version.processable!==true||state.unavailable;reference.onclick=()=>this.onReference(version.id);
    const download=node('a','下载','button');download.href=downloadURL(version,state.id);download.setAttribute('aria-label','下载 '+versionName(version));
    actions.append(reference);if(!state.unavailable)actions.append(download);card.append(preview,info,actions);return card;
  }
}
