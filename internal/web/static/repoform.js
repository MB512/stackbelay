// The repository form (issue #45). Plain script, no build step; without
// JavaScript every type section is shown and the server does the work
// (it also moves credentials out of a rest: URL on save).
//
// - Type radio buttons: only the section of the chosen type is shown.
// - "Move user and password …": takes user:password out of the URL field
//   (percent-decoded) and puts them into the REST fields, or into the
//   variables RESTIC_REST_USERNAME/RESTIC_REST_PASSWORD (Other). Enabled
//   only while the URL contains credentials.
// - Variables table: "Add variable" and × per row; a secret variable's
//   value field is a password field.
(function () {
	"use strict";

	// splitCredentials returns {url, user, pass} with user:password taken
	// out of an http(s) URL (optionally with "rest:" in front), or null if
	// it has none or cannot be parsed. Only the authority is touched, the
	// rest of the URL stays as typed.
	function splitCredentials(raw) {
		var s = raw.trim();
		var prefix = "";
		if (s.indexOf("rest:") === 0) {
			prefix = "rest:";
			s = s.slice(5);
		}
		var scheme = s.indexOf("://");
		if (scheme < 0) {
			return null;
		}
		var start = scheme + 3;
		var end = s.indexOf("/", start);
		if (end < 0) {
			end = s.length;
		}
		var authority = s.slice(start, end);
		var at = authority.lastIndexOf("@");
		if (at < 0) {
			return null;
		}
		var userinfo = authority.slice(0, at);
		var colon = userinfo.indexOf(":");
		var user = colon < 0 ? userinfo : userinfo.slice(0, colon);
		var pass = colon < 0 ? "" : userinfo.slice(colon + 1);
		try {
			user = decodeURIComponent(user);
			pass = decodeURIComponent(pass);
		} catch (e) {
			return null; // invalid percent-encoding: the server reports it
		}
		return {
			url: prefix + s.slice(0, start) + authority.slice(at + 1) + s.slice(end),
			user: user,
			pass: pass
		};
	}

	function setup(form) {
		var radios = form.querySelectorAll('input[name="type"]');
		var sections = form.querySelectorAll("fieldset[data-type]");
		var rows = form.querySelector("[data-env-rows]");
		var template = form.querySelector("template[data-env-template]");

		function showType() {
			var checked = form.querySelector('input[name="type"]:checked');
			var type = checked ? checked.value : "";
			sections.forEach(function (sec) {
				sec.hidden = sec.getAttribute("data-type") !== type;
			});
		}
		radios.forEach(function (r) { r.addEventListener("change", showType); });
		showType();

		// Variables table.
		function setupRow(tr) {
			var key = tr.querySelector("[data-env-key]");
			var value = tr.querySelector("[data-env-value]");
			var remove = tr.querySelector("[data-remove-row]");
			key.addEventListener("change", function () {
				var opt = key.options[key.selectedIndex];
				value.type = opt && opt.hasAttribute("data-secret") ? "password" : "text";
			});
			remove.hidden = false;
			remove.addEventListener("click", function () { tr.remove(); });
		}
		function addRow() {
			var tr = template.content.querySelector("[data-env-row]").cloneNode(true);
			rows.appendChild(tr);
			setupRow(tr);
			return tr;
		}
		// rowFor returns the row with key, else an empty row (or a new one)
		// set to key.
		function rowFor(key) {
			var all = rows.querySelectorAll("[data-env-row]");
			var empty = null;
			for (var i = 0; i < all.length; i++) {
				var sel = all[i].querySelector("[data-env-key]");
				if (sel.value === key) {
					return all[i];
				}
				if (sel.value === "" && empty === null) {
					empty = all[i];
				}
			}
			var tr = empty || addRow();
			var select = tr.querySelector("[data-env-key]");
			select.value = key;
			select.dispatchEvent(new Event("change"));
			return tr;
		}
		if (rows && template) {
			rows.querySelectorAll("[data-env-row]").forEach(setupRow);
			var add = form.querySelector("[data-add-row]");
			add.hidden = false;
			add.addEventListener("click", function () { addRow().querySelector("[data-env-key]").focus(); });
		}

		// "Move user and password" next to each URL field.
		form.querySelectorAll("[data-url]").forEach(function (input) {
			var button = input.parentNode.querySelector("[data-move-credentials]");
			var kind = input.getAttribute("data-url");
			function update() {
				var v = input.value.trim();
				// Other: the server only takes credentials out of rest: URLs.
				var applies = kind === "rest" || v.indexOf("rest:") === 0;
				button.disabled = !applies || splitCredentials(v) === null;
			}
			button.hidden = false;
			input.addEventListener("input", update);
			update();
			button.addEventListener("click", function () {
				var c = splitCredentials(input.value);
				if (c === null) {
					return;
				}
				input.value = c.url;
				if (kind === "rest") {
					var sec = input.closest("fieldset");
					sec.querySelector("[data-cred-user]").value = c.user;
					sec.querySelector("[data-cred-pass]").value = c.pass;
				} else if (rows && template) {
					rowFor("RESTIC_REST_USERNAME").querySelector("[data-env-value]").value = c.user;
					rowFor("RESTIC_REST_PASSWORD").querySelector("[data-env-value]").value = c.pass;
				}
				update();
			});
		});
	}

	document.querySelectorAll("[data-repo-form]").forEach(function (div) {
		setup(div.closest("form"));
	});
})();
