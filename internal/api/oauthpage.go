// oauthpage.go：0.2.12 授权/登录页内嵌 HTML（服务端自渲染，无外部资源——
// CSP 友好、离线可用）。同一页面承接两种形态：mode=authz（授权码流：账密 →
// 按需二次验证 → 回跳 redirect）与 mode=device（设备码批准：用户码 + 账密 →
// 批准）。页面 JS 只调本服务端 JSON 端点（同源；Cookie 均 HttpOnly+
// SameSite=Strict）。渲染走字符串替换占位符：注入的 params 是**服务端自产
// 的合法 JSON**（账户输入不进 HTML——用户值一律经 JS textContent 落 DOM），
// 因此无需 html/template 的上下文转义。
package api

import (
	"encoding/json"
	"html"
	"strings"
)

// oauthLoginPageHTML 渲染登录/批准页。mode ∈ {authz, device}；params 服务端组装。
func oauthLoginPageHTML(mode string, params map[string]any) string {
	pj, _ := json.Marshal(params)
	if mode != "device" {
		mode = "authz"
	}
	r := strings.NewReplacer("__MODE__", mode, "__PARAMS__", string(pj))
	return r.Replace(oauthPageBody)
}

// oauthErrorPageHTML 渲染参数校验失败错误页（msg 服务端文案，HTML 转义兜底）。
func oauthErrorPageHTML(msg string) string {
	return strings.Replace(oauthErrorBody, "{{MSG}}", html.EscapeString(msg), 1)
}

const oauthErrorBody = `<!doctype html><html lang="zh"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>FileManage 登录</title>
<meta name="robots" content="noindex">
<style>body{font-family:system-ui,'PingFang SC','Microsoft YaHei',sans-serif;background:#f6f7fb;margin:0;
display:flex;min-height:100vh;align-items:center;justify-content:center}
.card{background:#fff;border-radius:14px;box-shadow:0 6px 24px rgba(15,20,40,.08);padding:28px 32px;max-width:420px;width:92%}
h1{font-size:17px;margin:0 0 10px}p{font-size:14px;color:#555;line-height:1.6}</style></head>
<body><div class="card"><h1>登录请求无效</h1><p>{{MSG}}</p>
<p>请回到客户端重新发起登录。</p></div></body></html>`

const oauthPageBody = `<!doctype html><html lang="zh"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>FileManage 登录</title>
<meta name="robots" content="noindex">
<style>
body{font-family:system-ui,'PingFang SC','Microsoft YaHei',sans-serif;background:#f6f7fb;margin:0;
display:flex;min-height:100vh;align-items:center;justify-content:center}
.card{background:#fff;border-radius:14px;box-shadow:0 6px 24px rgba(15,20,40,.08);padding:28px 32px;max-width:420px;width:92%}
h1{font-size:17px;margin:0 0 4px}p.sub{font-size:12.5px;color:#888;margin:0 0 14px;line-height:1.5}
label{display:block;font-size:13px;color:#444;margin:12px 0 4px}
input{box-sizing:border-box;width:100%;padding:10px 12px;border:1px solid #d8dbe4;border-radius:8px;font-size:14px}
input:focus{outline:none;border-color:#4f6ef7}
.btn{margin-top:20px;width:100%;padding:11px;border:none;border-radius:8px;background:#4f6ef7;color:#fff;
font-size:15px;cursor:pointer}.btn:disabled{opacity:.55}
.err{color:#d33;font-size:13px;margin-top:12px;min-height:18px}
.ok{color:#0a8a4a;font-size:13px;margin-top:8px;min-height:18px}
.totp{margin-top:10px;font-size:12.5px;color:#555;background:#f2f4fb;border-radius:8px;padding:10px 12px;word-break:break-all;line-height:1.6}
.hide{display:none}
</style></head>
<body><div class="card">
<h1>FileManage 登录</h1>
<p class="sub" id="sub"></p>
<div id="deviceBox">
  <label>用户码</label><input id="uc" autocomplete="off" spellcheck="false" placeholder="XXXX-XXXX">
</div>
<label>账户</label><input id="user" autocomplete="username" spellcheck="false">
<label>密码</label><input id="pass" type="password" autocomplete="current-password">
<div id="mfaBox" class="hide">
  <label id="mfaLabel">二次验证码</label>
  <div id="enroll" class="totp hide"></div>
  <input id="code" autocomplete="one-time-code" inputmode="numeric" spellcheck="false" placeholder="6 位验证码">
</div>
<button class="btn" id="go">登 录</button>
<div class="err" id="err"></div>
<div class="ok hide" id="ok"></div>
</div>
<script>
"use strict";
var P = __PARAMS__;
var MODE = "__MODE__";
var flowId = "";
function $(i){return document.getElementById(i);}
function showErr(m){$("ok").classList.add("hide");$("err").textContent = m || "请求失败";$("go").disabled=false;}
function showSucc(m){$("err").textContent="";var o=$("ok");o.textContent=m;o.classList.remove("hide");$("go").disabled=false;}
$("sub").textContent = MODE==="device"
  ? ("设备显示的用户码为 " + (P.userCode||"") + "，输入您的账户密码以批准该设备登录。")
  : ("正在为「" + (P.clientName||"客户端") + "」登录 FileManage 账户。");
if (MODE==="device"){ if(P.userCode){$("uc").value=P.userCode;} } else { $("deviceBox").classList.add("hide"); }
$("go").addEventListener("click", submit);
function enterHook(el){el.addEventListener("keydown",function(e){if(e.key==="Enter")submit();});}
enterHook($("pass"));enterHook($("code"));enterHook($("user"));enterHook($("uc"));
function submit(){
  $("err").textContent="";$("go").disabled=true;
  var u=$("user").value.trim(), p=$("pass").value;
  if(!u||!p){showErr("请输入账户与密码");return;}
  if(MODE==="device" && !$("uc").value.trim()){showErr("请输入用户码");return;}
  var path, body;
  if($("mfaBox").classList.contains("hide")){
    body = MODE==="device"
      ? {userCode:$("uc").value.trim(), username:u, password:p}
      : {clientId:P.clientId, challenge:P.challenge, method:P.method, redirect:P.redirect,
         state:P.state, nonce:P.nonce, scope:P.scope, username:u, password:p};
    path = P.base + "/" + (MODE==="device" ? "device/verify" : "login");
  }else{
    body = {flowId:flowId, code:$("code").value.trim()};
    path = P.base + "/mfa";
  }
  fetch(path,{method:"POST",headers:{"Content-Type":"application/json"},
    credentials:"same-origin",body:JSON.stringify(body)})
    .then(handle).catch(function(){showErr("网络错误，请重试");});
}
function handle(resp){
  resp.json().then(function(j){
    if(resp.status===200){
      if(j.ok && j.redirect){location.href=j.redirect;return;}
      if(j.done){$("go").classList.add("hide");$("user").disabled=true;$("pass").disabled=true;
        showSucc("已批准该设备的登录请求，请回到设备继续。");return;}
      if(j.sent){showSucc("验证码已重新发送（5 分钟内有效）");return;}
      if(j.mfa){
        flowId=j.flowId||"";
        $("mfaBox").classList.remove("hide");
        if(j.mfa==="email"){
          $("mfaLabel").textContent="邮箱验证码（收件邮箱见下方提示；点按钮可重发）";
          $("go").textContent="验 证";
        }else{
          $("mfaLabel").textContent="验证器动态码（6 位）";
          $("go").textContent = j.enroll ? "确认开通并验证" : "验 证";
        }
        if(j.hint){showSucc(j.hint);}
        if(j.enroll){
          var e=$("enroll");e.classList.remove("hide");
          e.textContent="首次开通：请在验证器 App 手动新建账户（本服务不提供二维码图，密钥形如下方 code 所示），随后输入当前 6 位码确认开通。";
          appendLine(e,"密钥："+j.enroll.secret);
          appendLine(e,"otpauth："+j.enroll.otpauth);
        }
        if(j.sendError){showErr("邮件投递失败：" + j.sendError);}
        return;
      }
    }
    showErr(j.error||("请求失败（HTTP "+resp.status+"）"));
  },function(){showErr("响应解析失败");});
}
function appendLine(box,text){var d=document.createElement("div");d.textContent=text;box.appendChild(d);}
</script></body></html>`
