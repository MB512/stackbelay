// Live view of a job page (templates/job.html).
//
// Follows /jobs/<id>/events (Server-Sent Events): "line" events are
// appended to the log, "details" events replace the details section, "done"
// ends the stream. Every line event carries its end offset in log.txt as
// event id; after a lost connection (e.g. the manager is stopped by its own
// backup) the script reconnects with ?offset=<last id> and backoff, so no
// line is shown twice or missed. Before reconnecting it checks the job page
// and stops if the job is unknown (404) or the session has ended. Plain script, no build step; the HTML in
// the events is rendered and escaped by the server (html/template).
(function () {
	"use strict";

	var job = document.getElementById("job");
	if (!job || job.hasAttribute("data-final") || !window.EventSource) {
		return;
	}
	var url = job.getAttribute("data-events");
	var offset = job.getAttribute("data-offset") || "0";
	var log = document.getElementById("job-log");
	var live = document.getElementById("job-live");
	var delay = 1000;
	var done = false;
	var source = null;

	function status(text) {
		if (live) {
			live.textContent = text;
		}
	}

	function remember(e) {
		if (e.lastEventId) {
			offset = e.lastEventId;
		}
	}

	function connect() {
		source = new EventSource(url + "?offset=" + encodeURIComponent(offset));

		source.onopen = function () {
			delay = 1000;
			status("live");
		};

		source.addEventListener("line", function (e) {
			var atBottom = log.scrollTop + log.clientHeight >= log.scrollHeight - 8;
			log.insertAdjacentHTML("beforeend", e.data);
			remember(e);
			var empty = document.getElementById("job-log-empty");
			if (empty) {
				empty.remove();
			}
			if (atBottom) {
				log.scrollTop = log.scrollHeight;
			}
		});

		source.addEventListener("offset", remember);

		source.addEventListener("details", function (e) {
			var details = document.getElementById("job-details");
			if (details) {
				details.outerHTML = e.data;
			}
		});

		source.addEventListener("done", function () {
			done = true;
			source.close();
			status("");
		});

		// The browser's own reconnect gives up on HTTP errors (e.g. 502 from
		// a reverse proxy while the manager is down), so reconnect here.
		source.onerror = function () {
			source.close();
			if (done) {
				return;
			}
			status("connection lost, reconnecting…");
			window.setTimeout(retry, delay);
			delay = Math.min(delay * 2, 15000);
		};
	}

	// retry asks the job page first: there is no point in reconnecting to
	// a job that no longer exists or when the session has ended.
	function retry() {
		window.fetch(window.location.pathname, {credentials: "same-origin", redirect: "manual", cache: "no-store"})
			.then(function (resp) {
				if (resp.status === 404) {
					status("this job is not known (any more); reload the page");
				} else if (resp.type === "opaqueredirect" || resp.status === 401) {
					status("logged out; reload the page to log in again");
				} else if (resp.ok) {
					connect();
				} else {
					source.onerror(); // e.g. 502 while the manager is down
				}
			})
			.catch(function () {
				source.onerror(); // manager not reachable
			});
	}

	connect();
})();
