package browser

// pages are the fixture documents served by the test site, by path.
// {{OTHER}} expands to the base URL of the second origin.
var pages = map[string]string{
	"/": `<title>Home</title>
<h1>Welcome</h1>
<p>Hello <b>bold</b> world</p>
<a href="/form">Sign in</a>
<ul><li>One</li><li>Two</li></ul>
<div><div><button disabled>Unavailable</button></div></div>
<div style="display:none">Hidden text</div>
<img src="data:image/gif;base64,R0lGODlhAQABAAAAACw=" alt="Logo">`,

	"/form": `<title>Sign in</title>
<h1>Sign in</h1>
<form action="/submit" method="post">
<label>Email <input name="email" type="email"></label>
<label>Password <input name="password" type="password"></label>
<label>Code <input name="code"></label>
<label><input type="checkbox" name="remember"> Remember me</label>
<label><input type="checkbox" name="news" checked> Newsletter</label>
<select name="color" aria-label="Color"><option value="r">Red</option><option value="g">Green</option><option value="b">Blue</option></select>
<input type="date" name="when" aria-label="When">
<input type="range" name="volume" aria-label="Volume" min="0" max="10" value="5">
<input type="color" name="tint" aria-label="Tint">
<textarea name="notes" aria-label="Notes"></textarea>
<label><input type="radio" name="plan" value="free"> Free</label>
<label><input type="radio" name="plan" value="pro"> Pro</label>
<input name="locked" aria-label="Locked" value="fixed" readonly>
<button type="submit">Submit</button>
</form>
<div contenteditable="true" aria-label="Bio" id="bio" style="min-height:20px;border:1px solid"></div>
<a href="/reset">Forgot password?</a>`,

	"/links": `<title>Links</title>
<a href="/nocontent">No content</a>
<a href="#section">Jump</a>
<a href="mailto:someone@example.com">Mail</a>
<a href="javascript:void(document.title='script ran')">Script</a>
<a href="/slow">Slow page</a>
<form action="/submit"><input name="q" aria-label="Query"><button>Search</button></form>
<h2 id="section">Section</h2>`,

	"/reset": `<title>Reset</title><h1>Reset your password</h1><a href="/">Home</a>`,

	"/delayed": `<title>Delayed</title>
<p id="out">waiting</p>
<script>
setTimeout(() => {
  const b = document.createElement('button');
  b.textContent = 'Late button';
  b.disabled = true;
  b.onclick = () => { document.getElementById('out').textContent = 'late clicked'; };
  document.body.append(b);
  setTimeout(() => { b.disabled = false; }, 400);
}, 300);
</script>`,

	"/covered": `<title>Covered</title>
<button onclick="document.title='clicked'">Buy now</button>
<div class="cookie-banner" style="position:fixed;inset:0;background:rgba(0,0,0,.5)">We use cookies</div>`,

	"/sticky": `<title>Sticky</title>
<style>header{position:sticky;top:0;height:120px;background:#ccc}main div{height:300px}</style>
<header>Sticky header</header>
<main>
<div>one</div><div>two</div><div>three</div><div>four</div><div>five</div>
<button id="deep" onclick="document.getElementById('out').textContent='deep clicked'">Deep button</button>
<div>six</div><div>seven</div><div>eight</div>
<p id="out">idle</p>
<button id="top" onclick="document.getElementById('out').textContent='top clicked'">Upper button</button>
<div>nine</div><div>ten</div><div>eleven</div>
</main>`,

	"/frames": `<title>Frames</title>
<h1>Frames</h1>
<iframe src="/inner" title="same" width="400" height="120"></iframe>
<iframe src="{{OTHER}}/inner" title="cross" width="400" height="120" style="margin-left:40px;border:5px solid"></iframe>`,

	"/inner": `<title>Inner</title>
<button onclick="document.getElementById('out').textContent='inner clicked'">Inner button</button>
<input aria-label="Inner field" oninput="document.getElementById('echo').textContent='typed ' + this.value">
<p id="out">idle</p><p id="echo"></p>`,

	"/shadow": `<title>Shadow</title>
<div id="host"></div><p id="out">idle</p>
<script>
const root = document.getElementById('host').attachShadow({mode: 'open'});
root.innerHTML = '<button>Shadow button</button><input aria-label="Shadow field">';
root.querySelector('button').onclick = () => { document.getElementById('out').textContent = 'shadow clicked'; };
root.querySelector('input').oninput = e => { document.getElementById('out').textContent = 'shadow typed ' + e.target.value; };
</script>`,

	"/dialogs": `<title>Dialogs</title>
<button onclick="alert('Heads up'); out.textContent='alert done'">Alert</button>
<button onclick="out.textContent = 'confirm ' + confirm('Proceed?')">Confirm</button>
<button onclick="out.textContent = 'prompt ' + prompt('Your name?', 'nobody')">Prompt</button>
<p id="out">idle</p>
<script>const out = document.getElementById('out'); addEventListener('beforeunload', e => { e.preventDefault(); e.returnValue = 'x'; });</script>`,

	"/popup": `<title>Opener</title>
<a href="/reset" target="_blank">Open in new tab</a>`,

	"/upload-form": `<title>Upload</title>
<form action="/upload" method="post" enctype="multipart/form-data">
<input type="file" name="doc" aria-label="Document">
<input type="file" name="extra" id="hidden-input" style="display:none" multiple>
<button type="button" onclick="document.getElementById('hidden-input').click()">Choose extras</button>
<button type="submit">Send</button>
</form>`,

	"/download-page": `<title>Download</title><a href="/download">Get report</a>`,

	"/storage": `<title>Storage</title><p>storage page</p>
<script>if (!localStorage.getItem('theme')) localStorage.setItem('theme', 'dark'); sessionStorage.setItem('step', '3');</script>`,

	"/console": `<title>Console</title><p>console page</p>
<script>console.log('hello log'); console.warn('careful'); console.error('boom', {code: 7}); setTimeout(() => { throw new Error('uncaught failure'); }, 0);</script>`,

	"/drag": `<title>Drag</title>
<div id="src" draggable="true" style="width:80px;height:40px;background:#9cf">Card</div>
<div id="dst" style="width:200px;height:100px;margin-top:60px;background:#eee">Drop zone</div>
<p id="out">idle</p>
<script>
src.addEventListener('dragstart', e => e.dataTransfer.setData('text/plain', 'card-1'));
dst.addEventListener('dragover', e => e.preventDefault());
dst.addEventListener('drop', e => { e.preventDefault(); out.textContent = 'dropped ' + e.dataTransfer.getData('text/plain'); });
</script>`,

	"/long": `<title>Long</title><h1>Catalogue</h1><script>
for (let i = 0; i < 400; i++) { const b = document.createElement('button'); b.textContent = 'Item number ' + i; document.body.append(b); }
</script>`,

	"/spa": `<title>App</title>
<button onclick="setTimeout(() => { const p = document.createElement('p'); p.textContent = 'Results loaded'; document.body.append(p); document.getElementById('spin').remove(); }, 500)">Load</button>
<p id="spin">Spinner here</p>`,
}
