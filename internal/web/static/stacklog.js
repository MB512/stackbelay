// Live container logs of a stack (Logs tab) or of several stacks (global
// log view, /logs); the panel is "log-panel" in templates/partials.html.
//
// Follows the stream in data-events (Server-Sent Events). "line" events
// carry one server-rendered, escaped line with data-c = container name
// (global view: "<stack>/<container>") and the line's time as event id and
// data-t; lines are inserted in time order. The container checkboxes show
// or hide lines in the browser only. When the stream ends ("end",
// "failed", or the connection was lost) the script reconnects with
// ?since=<last id> and backoff, so lines are not repeated. Plain script,
// no build step.
(function () {
	"use strict";

	var root = document.getElementById("stack-logs");
	if (!root || root.hasAttribute("data-unavailable") || !window.EventSource) {
		return;
	}
	var url = root.getAttribute("data-events");
	var log = document.getElementById("stack-log");
	var live = document.getElementById("stack-log-live");
	var boxes = document.getElementById("log-containers");
	var maxLines = 5000;
	var hidden = {}; // container name -> true while its checkbox is off
	var since = "";
	var delay = 1000;
	var source = null;

	function status(text) {
		live.textContent = text;
	}

	function applyFilter(line) {
		line.hidden = hidden[line.getAttribute("data-c")] === true;
	}

	function setShown(name, shown) {
		if (shown) {
			delete hidden[name];
		} else {
			hidden[name] = true;
		}
		var lines = log.children;
		for (var i = 0; i < lines.length; i++) {
			if (lines[i].getAttribute("data-c") === name) {
				applyFilter(lines[i]);
			}
		}
	}

	function checkboxes() {
		return boxes.querySelectorAll("input[type=checkbox]");
	}

	// Container colors: each container gets one of the classes cc0..cc7
	// (style.css), in the order of its checkbox; the checkbox shows a
	// swatch of the same color as a legend.
	var colorCount = 8;
	var colors = {}; // container name -> class
	function addColor(input) {
		var cls = "cc" + (Object.keys(colors).length % colorCount);
		colors[input.value] = cls;
		var swatch = document.createElement("span");
		swatch.className = "swatch " + cls;
		swatch.setAttribute("aria-hidden", "true");
		input.parentNode.insertBefore(swatch, input.nextSibling);
	}
	(function () {
		var all = checkboxes();
		for (var i = 0; i < all.length; i++) {
			addColor(all[i]);
		}
	})();
	function colorLine(line) {
		var cls = colors[line.getAttribute("data-c")];
		if (cls) {
			line.classList.add("colored", cls);
		}
	}

	// Containers whose checkbox starts unchecked (the page was opened for
	// one container) are hidden from the start.
	(function () {
		var all = checkboxes();
		for (var i = 0; i < all.length; i++) {
			if (!all[i].checked) {
				hidden[all[i].value] = true;
			}
		}
	})();

	boxes.addEventListener("change", function (e) {
		if (e.target.type === "checkbox") {
			setShown(e.target.value, e.target.checked);
		}
	});

	function setAll(shown) {
		var all = checkboxes();
		for (var i = 0; i < all.length; i++) {
			all[i].checked = shown;
			setShown(all[i].value, shown);
		}
	}
	document.getElementById("log-show-all").addEventListener("click", function () { setAll(true); });
	document.getElementById("log-hide-all").addEventListener("click", function () { setAll(false); });

	// keepBottom runs a layout change and keeps the log scrolled to the end
	// if it was there before.
	function keepBottom(change) {
		var atBottom = log.scrollTop + log.clientHeight >= log.scrollHeight - 8;
		change();
		if (atBottom) {
			log.scrollTop = log.scrollHeight;
		}
	}

	// Line wrapping. The choice is remembered in this browser only; storage
	// may be unavailable (private mode), which is fine.
	// Reads a remembered choice; choices remembered before StackVault was
	// renamed to StackBelay (issue #44) are under the old key.
	function remembered(key) {
		var v = window.localStorage.getItem(key);
		return v !== null ? v : window.localStorage.getItem(key.replace(/^stackbelay\./, "stackvault."));
	}
	var wrapKey = "stackbelay.logs.wrap";
	var wrap = document.getElementById("log-wrap");
	function setWrap(on) {
		wrap.checked = on;
		keepBottom(function () { log.classList.toggle("nowrap", !on); });
		try {
			window.localStorage.setItem(wrapKey, on ? "1" : "0");
		} catch (e) { /* not remembered */ }
	}
	try {
		if (remembered(wrapKey) === "0") {
			setWrap(false);
		}
	} catch (e) { /* default: wrap */ }
	wrap.addEventListener("change", function () { setWrap(wrap.checked); });

	// ANSI colors of the applications (issue #29), like line wrapping:
	// remembered in this browser only.
	var colorsKey = "stackbelay.logs.colors";
	var colorsBox = document.getElementById("log-colors");
	function setColors(on) {
		colorsBox.checked = on;
		log.classList.toggle("nocolor", !on);
		try {
			window.localStorage.setItem(colorsKey, on ? "1" : "0");
		} catch (e) { /* not remembered */ }
	}
	try {
		if (remembered(colorsKey) === "0") {
			setColors(false);
		}
	} catch (e) { /* default: colors */ }
	colorsBox.addEventListener("change", function () { setColors(colorsBox.checked); });

	// Full screen inside the browser window (not the browser's own full
	// screen mode): the frame covers the page and shows only the log and a
	// bar with the container filter and the exit button. The filter element
	// itself is moved into the bar and back, so its state and handlers stay.
	var frame = document.getElementById("stack-log-frame");
	var bar = document.getElementById("log-bar");
	var enter = document.getElementById("log-fullscreen");
	var exit = document.getElementById("log-exit-fullscreen");
	var boxesHome = boxes.nextElementSibling; // the filter goes back before it
	function setFullscreen(on) {
		keepBottom(function () {
			if (on) {
				bar.insertBefore(boxes, exit);
			} else {
				boxesHome.parentNode.insertBefore(boxes, boxesHome);
			}
			frame.classList.toggle("fullscreen", on);
			document.body.classList.toggle("log-fullscreen", on);
			bar.hidden = !on;
		});
		(on ? exit : enter).focus();
	}
	enter.addEventListener("click", function () { setFullscreen(true); });
	exit.addEventListener("click", function () { setFullscreen(false); });
	document.addEventListener("keydown", function (e) {
		if (e.key === "Escape" && frame.classList.contains("fullscreen")) {
			setFullscreen(false);
		}
	});

	// addCheckbox adds a container that appeared after the page was loaded.
	function addCheckbox(name) {
		var all = checkboxes();
		for (var i = 0; i < all.length; i++) {
			if (all[i].value === name) {
				return;
			}
		}
		var empty = document.getElementById("log-containers-empty");
		if (empty) {
			empty.remove();
		}
		var label = document.createElement("label");
		label.className = "check";
		var input = document.createElement("input");
		input.type = "checkbox";
		input.value = name;
		input.checked = true;
		label.appendChild(input);
		label.appendChild(document.createTextNode(" " + name));
		boxes.insertBefore(label, boxes.querySelector(".actions"));
		addColor(input);
	}

	// insertLine adds a server-rendered (escaped) line in time order.
	// "compose logs" sends the history container by container and only
	// mixes the lines it follows live, so a line can be older than the
	// ones already shown. data-t has a fixed width (see logLineIDFormat in
	// stacklogs.go), so string comparison orders by time. Lines without
	// time (compose's own messages) stay where they arrived.
	function insertLine(html) {
		var tmp = document.createElement("div");
		tmp.innerHTML = html;
		var line = tmp.firstElementChild;
		var t = line.getAttribute("data-t");
		var before = null;
		if (t) {
			for (var n = log.lastElementChild; n; n = n.previousElementSibling) {
				var nt = n.getAttribute("data-t");
				if (!nt || nt <= t) {
					break;
				}
				before = n;
			}
		}
		log.insertBefore(line, before);
		return line;
	}

	function connect() {
		if (!since) {
			status("connecting…");
		}
		source = new EventSource(since ? url + "?since=" + encodeURIComponent(since) : url);

		// Until the first line arrives (compose may take a moment for the
		// history), say that the view is waiting.
		source.onopen = function () {
			status(log.children.length ? "live" : "live, waiting for the first lines…");
		};

		source.addEventListener("line", function (e) {
			delay = 1000;
			if (log.children.length === 0) {
				status("live");
			}
			var atBottom = log.scrollTop + log.clientHeight >= log.scrollHeight - 8;
			var line = insertLine(e.data);
			var name = line.getAttribute("data-c");
			if (name) {
				addCheckbox(name);
			}
			colorLine(line);
			applyFilter(line);
			// Resume after the newest line, not the last one received: the
			// history arrives container by container.
			if (e.lastEventId && e.lastEventId > since) {
				since = e.lastEventId;
			}
			while (log.children.length > maxLines) {
				log.firstElementChild.remove();
			}
			if (atBottom) {
				log.scrollTop = log.scrollHeight;
			}
		});

		source.addEventListener("end", function () {
			reconnectLater("log stream ended (no container running?); reconnecting…");
		});

		source.addEventListener("failed", function (e) {
			reconnectLater("error: " + e.data + "; retrying…");
		});

		// Connection lost or refused (e.g. too many open log views, or the
		// manager is restarting). The browser's own retry gives up on HTTP
		// errors, so retry here.
		source.onerror = function () {
			reconnectLater("connection lost, reconnecting…");
		};
	}

	function reconnectLater(text) {
		if (source) {
			source.close();
			source = null;
		} else {
			return; // already waiting
		}
		status(text);
		window.setTimeout(retry, delay);
		delay = Math.min(delay * 2, 30000);
	}

	// retry checks the page first: no point in reconnecting when the
	// session has ended or the right was taken away.
	function retry() {
		window.fetch(window.location.pathname + window.location.search, {credentials: "same-origin", redirect: "manual", cache: "no-store"})
			.then(function (resp) {
				if (resp.type === "opaqueredirect" || resp.status === 401) {
					status("logged out; reload the page to log in again");
				} else if (resp.status === 403 || resp.status === 404) {
					status("no access to these logs any more");
				} else {
					connect();
				}
			})
			.catch(function () {
				connect(); // manager not reachable: connect fails and backs off again
			});
	}

	connect();
})();
