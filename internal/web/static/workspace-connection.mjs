import {conversationPath} from './workspace-api.mjs';

export const SNAPSHOT_FRESHNESS_MS=15000;
export const connectionLabels={syncing:'正在同步进度',connected:'进度已连接',reconnecting:'连接中断，正在重连'};

// 连接代次隔离失效的 HTTP、WS 和计时器；同一代还必须匹配当前 socket。
export class ConversationConnection {
  constructor({load,onSnapshot,onStatus,onUnavailable,createSocket=url=>new WebSocket(url),origin=()=>location.origin,setTimer=(callback,delay)=>setTimeout(callback,delay),clearTimer=timer=>clearTimeout(timer),now=()=>Date.now(),freshnessMs=SNAPSHOT_FRESHNESS_MS}) {
    Object.assign(this,{load,onSnapshot,onStatus,onUnavailable,createSocket,origin,setTimer,clearTimer,now,freshnessMs});
    this.generation=0;this.socket=null;this.timer=null;this.freshnessTimer=null;this.id=null;this.cursor=0;
    this.phase='syncing';this.lastSnapshotAt=null;this.expiresAt=0;
  }
  status(phase=this.phase,lastSnapshotAt=this.lastSnapshotAt) {return {conversationID:this.id,generation:this.generation,phase,lastSnapshotAt};}
  notify(phase) {this.phase=phase;this.onStatus(this.status());}
  disposeSocket() {
    const socket=this.socket;this.socket=null;
    if(socket){socket.onclose=socket.onerror=socket.onmessage=socket.onopen=null;socket.close();}
  }
  clearTimers() {
    this.clearTimer(this.timer);this.clearTimer(this.freshnessTimer);this.timer=this.freshnessTimer=null;
  }
  close() {this.generation++;this.id=null;this.clearTimers();this.disposeSocket();this.lastSnapshotAt=null;}
  async open(id,initial) {
    this.close();this.id=id;this.cursor=0;const generation=this.generation;
    this.notify('syncing');
    if(initial?.conversation?.id===id)this.accept(initial,generation);
    await this.refresh(generation);
  }
  valid(generation,socket) {return generation===this.generation && !!this.id && (!socket||socket===this.socket);}
  accept(snapshot,generation,socket) {
    const cursor=snapshot?.cursor;
    if(!this.valid(generation,socket)||snapshot.conversation?.id!==this.id||!Number.isSafeInteger(cursor)||cursor<this.cursor)return false;
    const received=this.now(),phase=socket?'connected':'syncing';
    // POST 可能已带来更高游标；页面拒绝的旧快照不能刷新连接健康时间。
    if(this.onSnapshot(snapshot,this.status(phase,received))===false||!this.valid(generation,socket))return false;
    this.cursor=cursor;this.lastSnapshotAt=received;this.notify(phase);this.armFreshness(generation,received);return true;
  }
  armFreshness(generation,at=this.now()) {
    this.clearTimer(this.freshnessTimer);this.expiresAt=at+this.freshnessMs;
    this.freshnessTimer=this.setTimer(()=>{this.freshnessTimer=null;if(this.valid(generation))this.checkFreshness();},Math.max(0,this.expiresAt-this.now()));
  }
  checkFreshness() {if(this.id && this.now()>=this.expiresAt && this.timer===null)this.recover(this.generation);}
  async refresh(generation) {
    if(!this.valid(generation))return;
    this.armFreshness(generation);
    try {
      const data=await this.load(this.id);
      if(!this.valid(generation))return;
      if(!this.accept(data,generation)){this.recover(generation);return;}
      this.connect(generation);
    } catch(error) {
      if(!this.valid(generation))return;
      if(error.status===403||error.status===404){this.close();this.onUnavailable(error);return;}
      this.recover(generation);
    }
  }
  connect(generation) {
    if(!this.valid(generation))return;
    try {
      const socket=this.createSocket(this.origin().replace(/^http/,'ws')+conversationPath(this.id)+'/events?after='+this.cursor);
      this.socket=socket;
      // 握手成功不代表已取得当前进度，等待该 socket 的有效快照。
      socket.onopen=()=>{};
      socket.onmessage=event=>{
        if(!this.valid(generation,socket))return;
        try{this.accept(JSON.parse(event.data),generation,socket);}catch{this.recover(generation);}
      };
      socket.onclose=socket.onerror=()=>{if(this.valid(generation,socket))this.recover(generation);};
    } catch {this.recover(generation);}
  }
  recover(generation) {
    if(!this.valid(generation))return;
    this.generation++;this.clearTimers();this.disposeSocket();this.notify('reconnecting');
    const next=this.generation;
    this.timer=this.setTimer(()=>{this.timer=null;if(this.valid(next))this.refresh(next);},1500);
  }
}
