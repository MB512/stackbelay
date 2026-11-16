// Terminal of the "Shell" tab (templates/stack_shell.html).
//
// xterm.js (vendored, window.Terminal) renders the terminal; the session
// runs over a WebSocket to /stacks/<name>/shell/ws: binary messages carry
// terminal data in both directions, a text message {"type":"resize",...}
// tells the server the terminal size. There is no reconnect: a new shell
// is a new session (reload the page). Plain script, no build step.
(function () {
	"use strict";

	var el = document.getElementById("terminal");
	var status = document.getElementById("shell-status");
	if (!el || !window.WebSocket || !window.Terminal || !window.FitAddon) {
		if (status) {
			status.textContent = "this browser cannot show the terminal";
		}
		return;
	}

	var term = new window.Terminal({
		cursorBlink: true,
		scrollback: 5000,
		fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
		fontSize: 14,
		theme: {background: "#11151b", foreground: "#d7dde5"}
	});
	var fit = new window.FitAddon.FitAddon();
	term.loadAddon(fit);
	term.open(el);
	fit.fit();
	term.focus();

	var url = new URL(el.getAttribute("data-socket"), window.location.href);
	url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
	url.searchParams.set("cols", term.cols);
	url.searchParams.set("rows", term.rows);
	var ws = new WebSocket(url.toString());
	ws.binaryType = "arraybuffer";
	var encoder = new TextEncoder();

	function send(data) {
		if (ws.readyState === WebSocket.OPEN) {
			ws.send(data);
		}
	}

	ws.onopen = function () {
		status.textContent = "connected";
	};
	ws.onmessage = function (e) {
		if (typeof e.data !== "string") {
			term.write(new Uint8Array(e.data));
		}
	};
	ws.onclose = function (e) {
		status.textContent = "session ended" + (e.reason ? ": " + e.reason : "") + " (reload the page for a new shell)";
		term.write("\r\n\x1b[90m[session ended]\x1b[0m\r\n");
		term.options.disableStdin = true;
	};

	// Keystrokes (UTF-8) and binary input such as mouse reports.
	term.onData(function (d) {
		send(encoder.encode(d));
	});
	term.onBinary(function (d) {
		var bytes = new Uint8Array(d.length);
		for (var i = 0; i < d.length; i++) {
			bytes[i] = d.charCodeAt(i) & 255;
		}
		send(bytes);
	});
	term.onResize(function (size) {
		send(JSON.stringify({type: "resize", cols: size.cols, rows: size.rows}));
	});
	window.addEventListener("resize", function () {
		fit.fit();
	});

	// Full screen inside the browser window (like the log view): the frame
	// covers the page; the terminal is fitted to its new size, which sends
	// the new size to the shell. Only the button exits: Esc goes to the shell.
	var frame = document.getElementById("shell-frame");
	var bar = document.getElementById("shell-bar");
	var enter = document.getElementById("shell-fullscreen");
	var exit = document.getElementById("shell-exit-fullscreen");
	function setFullscreen(on) {
		frame.classList.toggle("fullscreen", on);
		document.body.classList.toggle("term-fullscreen", on);
		bar.hidden = !on;
		fit.fit();
		term.focus();
	}
	enter.addEventListener("click", function () { setFullscreen(true); });
	exit.addEventListener("click", function () { setFullscreen(false); });
})();
