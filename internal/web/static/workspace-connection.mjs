import {conversationPath} from './workspace-api.mjs';

// 每次选择会话递增连接代次；旧 HTTP、WS 和重连定时器都不能覆盖当前页面。
export class ConversationConnection {
  constructor({load,onSnapshot,onStatus,onUnavailable,createSocket=url=>new WebSocket(url),origin=()=>location.origin,setTimer=(callback,delay)=>setTimeout(callback,delay),clearTimer=timer=>clearTimeout(timer)}) {
    Object.assign(this,{load,onSnapshot,onStatus,onUnavailable,createSocket,origin,setTimer,clearTimer});
    this.generation=0;this.socket=null;this.timer=null;this.id=null;this.cursor=0;
  }
  close() {
    this.generation++;this.id=null;this.clearTimer(this.timer);this.timer=null;
    if(this.socket){this.socket.onclose=null;this.socket.close();this.socket=null;}
  }
  async open(id,initial) {
    this.close();this.id=id;this.cursor=0;const generation=this.generation;
    this.onStatus('正在连接');
    if(initial?.conversation?.id===id)this.accept(initial,generation);
    await this.refresh(generation);
  }
  valid(generation){return generation===this.generation && !!this.id;}
  accept(snapshot,generation) {
    if(!this.valid(generation)||snapshot.conversation?.id!==this.id)return;
    this.cursor=Math.max(this.cursor,Number(snapshot.cursor||0));this.onSnapshot(snapshot);
  }
  async refresh(generation) {
    if(!this.valid(generation))return;
    try {
      const data=await this.load(this.id);
      if(!this.valid(generation))return;
      this.accept(data,generation);this.connect(generation);
    } catch(error) {
      if(!this.valid(generation))return;
      if(error.status===403||error.status===404){this.onUnavailable(error);this.close();return;}
      this.onStatus('连接中断，正在重连');this.schedule(generation);
    }
  }
  connect(generation) {
    if(!this.valid(generation))return;
    const origin=this.origin().replace(/^http/,'ws');
    const socket=this.createSocket(origin+conversationPath(this.id)+'/events?after='+this.cursor);
    this.socket=socket;
    socket.onopen=()=>{if(this.valid(generation))this.onStatus('进度已连接');};
    socket.onmessage=event=>{
      if(!this.valid(generation))return;
      try{this.accept(JSON.parse(event.data),generation);}catch{socket.close();}
    };
    socket.onclose=()=>{if(this.valid(generation)){this.onStatus('连接中断，正在重连');this.schedule(generation);}};
    socket.onerror=()=>socket.close();
  }
  schedule(generation) {
    this.clearTimer(this.timer);
    this.timer=this.setTimer(()=>this.refresh(generation),1500);
  }
}
