// Small form helpers for every page. Plain script, no build step; without
// JavaScript all forms work as they are (the server checks everything).
//
// - data-confirm="<question>": ask before submitting (e.g. stop or
//   restart a stack, an update without backup). With
//   data-confirm-when="<field>=<value>" only if that field of the form has
//   the value (e.g. the job type of a schedule).
// - data-enable-when="<field>=<value>" on an input: the input is enabled
//   only while that field of its form has the value, otherwise disabled
//   and unchecked (e.g. an update-only option of a schedule while
//   "Backup" is selected). The server rejects such an input anyway.
// - Busy state: a plain form that is submitted (the browser waits for the
//   next page) marks its submit button busy: a spinner (class "busy", see
//   style.css), disabled against double submits, and the text of the
//   form's data-busy attribute if it has one ("Checking access…"). HTMX
//   forms are left alone: HTMX sets the class "htmx-request" on them
//   during the request, which style.css shows the same way.
(function () {
	"use strict";

	// Countdown of the notification mute in the header (issue #50):
	// data-countdown="<seconds left>" shows m:ss and "ended" at 0. Without
	// JavaScript the element shows the end time.
	function startCountdowns() {
		var els = document.querySelectorAll("[data-countdown]");
		if (els.length === 0) {
			return;
		}
		var start = Date.now();
		function tick() {
			for (var i = 0; i < els.length; i++) {
				var left = Math.max(0, Number(els[i].getAttribute("data-countdown")) - Math.floor((Date.now() - start) / 1000));
				var s = left % 60;
				els[i].textContent = left === 0 ? "– ended" : Math.floor(left / 60) + ":" + (s < 10 ? "0" : "") + s;
			}
		}
		tick();
		setInterval(tick, 1000);
	}
	if (document.readyState === "loading") {
		document.addEventListener("DOMContentLoaded", startCountdowns);
	} else {
		startCountdowns();
	}

	// fieldIs reports whether form's field has a value, for a condition
	// "<field>=<value>". form.elements[name] of radio buttons is a list
	// whose value is the checked one's.
	function fieldIs(form, when) {
		var i = when.indexOf("=");
		var field = form.elements[when.slice(0, i)];
		return field !== undefined && field.value === when.slice(i + 1);
	}

	function confirmApplies(form) {
		var when = form.getAttribute("data-confirm-when");
		return !when || fieldIs(form, when);
	}

	function updateEnabled() {
		var inputs = document.querySelectorAll("[data-enable-when]");
		for (var i = 0; i < inputs.length; i++) {
			var input = inputs[i];
			var on = input.form !== null && fieldIs(input.form, input.getAttribute("data-enable-when"));
			input.disabled = !on;
			if (!on) {
				input.checked = false;
			}
		}
	}

	updateEnabled();
	document.addEventListener("change", updateEnabled);

	function markBusy(form, button) {
		if (!button) {
			return;
		}
		button.classList.add("busy");
		var text = form.getAttribute("data-busy");
		if (text) {
			button.setAttribute("data-label", button.textContent);
			button.textContent = text;
		}
		// Disable after the browser has taken the form data (a disabled
		// button would not be part of it).
		window.setTimeout(function () {
			button.disabled = true;
		}, 0);
	}

	document.addEventListener("submit", function (e) {
		var form = e.target;
		if (!form.getAttribute) {
			return;
		}
		var question = form.getAttribute("data-confirm");
		if (question && confirmApplies(form) && !window.confirm(question)) {
			e.preventDefault();
			return;
		}
		// HTMX prevents the default submit of its forms before this
		// listener runs (it listens on the form itself).
		if (e.defaultPrevented) {
			return;
		}
		markBusy(form, e.submitter || form.querySelector("button[type=submit], button:not([type])"));
	});

	// Back to a page from the browser's cache: undo busy buttons.
	window.addEventListener("pageshow", function (e) {
		if (!e.persisted) {
			return;
		}
		var busy = document.querySelectorAll("button.busy");
		for (var i = 0; i < busy.length; i++) {
			busy[i].classList.remove("busy");
			busy[i].disabled = false;
			if (busy[i].hasAttribute("data-label")) {
				busy[i].textContent = busy[i].getAttribute("data-label");
				busy[i].removeAttribute("data-label");
			}
		}
	});
})();
