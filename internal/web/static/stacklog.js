// Live container logs of a stack (templates/stack_logs.html).
//
// Follows /stacks/<name>/logs/events (Server-Sent Events). "line" events
// carry one server-rendered, escaped line with data-c = container name and
// the line's time as event id. The container checkboxes show or hide lines
// in the browser only. When the stream ends ("end", "failed", or the
// connection was lost) the script reconnects with ?since=<last id> and
// backoff, so lines are not repeated. Plain script, no build step.
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
	var wrapKey = "stackvault.logs.wrap";
	var wrap = document.getElementById("log-wrap");
	function setWrap(on) {
		wrap.checked = on;
		keepBottom(function () { log.classList.toggle("nowrap", !on); });
		try {
			window.localStorage.setItem(wrapKey, on ? "1" : "0");
		} catch (e) { /* not remembered */ }
	}
	try {
		if (window.localStorage.getItem(wrapKey) === "0") {
			setWrap(false);
		}
	} catch (e) { /* default: wrap */ }
	wrap.addEventListener("change", function () { setWrap(wrap.checked); });

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
	}

	function connect() {
		source = new EventSource(since ? url + "?since=" + encodeURIComponent(since) : url);

		source.onopen = function () {
			status("live");
		};

		source.addEventListener("line", function (e) {
			delay = 1000;
			var atBottom = log.scrollTop + log.clientHeight >= log.scrollHeight - 8;
			log.insertAdjacentHTML("beforeend", e.data);
			var line = log.lastElementChild;
			var name = line.getAttribute("data-c");
			if (name) {
				addCheckbox(name);
			}
			applyFilter(line);
			if (e.lastEventId) {
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
		window.fetch(window.location.pathname, {credentials: "same-origin", redirect: "manual", cache: "no-store"})
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
