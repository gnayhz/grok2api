/* Grok2API — i18n */
(function(){
  var KEY='grok2api_lang', LANGS=['zh','en'], lang='zh', data={}, ready=false, queue=[];

  function detect(){
    try{ var s=localStorage.getItem(KEY); if(s&&LANGS.indexOf(s)!==-1) return s; }catch(e){}
    var p; try{p=new URLSearchParams(location.search).get('lang')}catch(e){}
    if(p&&LANGS.indexOf(p)!==-1){try{localStorage.setItem(KEY,p)}catch(e){} return p;}
    var b=(navigator.language||'').split('-')[0];
    return LANGS.indexOf(b)!==-1?b:'zh';
  }

  function get(o,k){ for(var p=k.split('.'),i=0;i<p.length;i++){if(o==null)return; o=o[p[i]];} return o; }

  function t(k,p){
    var v=get(data,k); if(v===undefined) return k;
    if(p) Object.keys(p).forEach(function(n){v=v.replace(new RegExp('\\{'+n+'\\}','g'),p[n]);});
    return v;
  }

  function apply(root){
    var c=root||document;
    c.querySelectorAll('[data-i18n]').forEach(function(el){ var v=get(data,el.getAttribute('data-i18n')); if(v!==undefined) el.textContent=v; });
    c.querySelectorAll('[data-i18n-placeholder]').forEach(function(el){ var v=get(data,el.getAttribute('data-i18n-placeholder')); if(v!==undefined) el.placeholder=v; });
  }

  function init(){
    lang=detect();
    document.documentElement.lang=lang==='zh'?'zh-CN':lang;
    fetch('/static/i18n/'+lang+'.json').then(function(r){return r.ok?r.json():{}}).catch(function(){return{}}).then(function(j){
      data=j; ready=true; apply(document); queue.forEach(function(cb){cb()}); queue=[];
    });
  }

  function setLang(l){ if(LANGS.indexOf(l)===-1)return; try{localStorage.setItem(KEY,l)}catch(e){} location.reload(); }

  window.I18n={t:t, apply:apply, setLang:setLang, toggleLang:function(){setLang(lang==='zh'?'en':'zh')}, getLang:function(){return lang}, onReady:function(cb){if(ready)cb();else queue.push(cb)}};
  window.t=t;
  document.readyState==='loading'?document.addEventListener('DOMContentLoaded',init):init();
})();
