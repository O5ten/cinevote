// CineVote front-end: the movie search box and a two-step delete confirm.
// No dependencies, no build step.
(function () {
  "use strict";

  /* ---------------------------------------------------------- movie search */

  var form = document.getElementById("suggest-form");
  var button = document.getElementById("search-btn");
  var box = document.getElementById("search-results");

  if (form && box) {
    // Search fires on its own once typing pauses; the button is just a way to
    // skip the wait.
    var DEBOUNCE_MS = 600;
    var MIN_QUERY = 2;

    var title = document.getElementById("title");
    var year = document.getElementById("year");
    var posterURL = document.getElementById("poster_url");
    var overview = document.getElementById("overview");
    var sourceID = document.getElementById("source-id");
    var label = (button && button.dataset.label) || "filmdatabasen";
    var idle = button ? button.textContent : "";

    var timer = null;
    var inFlight = null;
    var lastQuery = "";

    title.addEventListener("input", function () {
      // Picking a result stops being valid as soon as the title is edited by
      // hand, otherwise we would attach the wrong film's poster.
      sourceID.value = "";
      schedule();
    });

    // Enter in the title field searches rather than submitting a half-filled
    // form, as long as we have something to search with.
    title.addEventListener("keydown", function (event) {
      if (event.key === "Enter" && title.value.trim().length >= MIN_QUERY && !sourceID.value) {
        event.preventDefault();
        searchNow();
      }
    });

    if (button) {
      button.addEventListener("click", searchNow);
    }

    function schedule() {
      window.clearTimeout(timer);
      var query = title.value.trim();
      if (query.length < MIN_QUERY) {
        hide();
        return;
      }
      if (query === lastQuery) return;
      timer = window.setTimeout(function () {
        run(query);
      }, DEBOUNCE_MS);
    }

    function searchNow() {
      window.clearTimeout(timer);
      var query = title.value.trim();
      if (!query) {
        title.focus();
        return;
      }
      run(query);
    }

    function run(query) {
      lastQuery = query;

      // A slow answer for an older query must not overwrite a newer one.
      if (inFlight) inFlight.abort();
      inFlight = new AbortController();
      var request = inFlight;

      busy(true);
      message("Söker på " + label + " efter \u201d" + query + "\u201d…");

      fetch("/api/search?q=" + encodeURIComponent(query), {
        headers: { Accept: "application/json" },
        credentials: "same-origin",
        signal: request.signal
      })
        .then(function (resp) {
          return resp.json().then(function (body) {
            if (!resp.ok) throw new Error(body.error || "Sökningen misslyckades.");
            return body;
          });
        })
        .then(function (body) {
          render(body.results || []);
        })
        .catch(function (err) {
          if (err.name === "AbortError") return; // superseded by a newer search
          message(err.message || "Sökningen misslyckades. Fyll i uppgifterna själv.");
        })
        .finally(function () {
          if (inFlight === request) {
            inFlight = null;
            busy(false);
          }
        });
    }

    function busy(on) {
      if (!button) return;
      button.disabled = on;
      button.textContent = on ? "Söker…" : idle;
    }

    function hide() {
      box.hidden = true;
      box.textContent = "";
      open(false);
    }

    // open toggles the "attached dropdown" look and the combobox state.
    function open(isOpen) {
      var wrap = box.closest(".searchfield");
      if (wrap) wrap.classList.toggle("open", isOpen);
      title.setAttribute("aria-expanded", isOpen ? "true" : "false");
    }

    function message(text) {
      box.hidden = false;
      open(true);
      box.textContent = "";
      var p = document.createElement("p");
      p.className = "results-msg";
      p.textContent = text;
      box.appendChild(p);
    }

    function render(results) {
      box.hidden = false;
      open(true);
      box.textContent = "";

      if (!results.length) {
        message("Inga träffar på " + label + ". Fyll i uppgifterna själv.");
        return;
      }

      results.forEach(function (movie) {
        var row = document.createElement("button");
        row.type = "button";
        row.className = "result";
        row.title = movie.title + (movie.year ? " (" + movie.year + ")" : "");

        row.appendChild(thumbnail(movie));

        var wrap = document.createElement("span");
        wrap.className = "result-label";

        var name = document.createElement("span");
        name.className = "result-title";
        name.textContent = movie.title;
        wrap.appendChild(name);

        var sub = document.createElement("span");
        sub.className = "result-year";
        sub.textContent = [movie.year, movie.rating ? "\u2605 " + movie.rating : "", movie.genres]
          .filter(Boolean)
          .join(" · ");
        wrap.appendChild(sub);

        row.appendChild(wrap);
        row.addEventListener("click", function () {
          select(movie);
        });
        box.appendChild(row);
      });
    }

    // Escape closes the list, and so does clicking anywhere outside it — the
    // usual way out of an autocomplete.
    title.addEventListener("keydown", function (event) {
      if (event.key === "Escape" && !box.hidden) {
        event.stopPropagation();
        hide();
      }
    });
    document.addEventListener("click", function (event) {
      if (box.hidden) return;
      var wrap = box.closest(".searchfield");
      if (wrap && !wrap.contains(event.target)) hide();
    });

    // select fills the form from a search hit. source_id is what the server
    // re-resolves against OMDb, so the browser cannot invent metadata.
    function select(movie) {
      window.clearTimeout(timer);
      sourceID.value = movie.imdb_id || (movie.tmdb_id ? String(movie.tmdb_id) : "");
      title.value = movie.title;
      lastQuery = movie.title.trim();
      if (movie.year) year.value = movie.year;
      if (movie.poster_url) posterURL.value = movie.poster_url;
      if (movie.overview) overview.value = movie.overview;
      message("Valt: " + movie.title + (movie.year ? " (" + movie.year + ")" : "") + ".");
    }

    // thumbnail is the poster, or a coloured placeholder — including when the
    // poster URL turns out to be dead, which OMDb has plenty of.
    function thumbnail(movie) {
      if (!movie.poster_url) return placeholder(movie.title);

      var img = document.createElement("img");
      img.className = "poster";
      img.src = movie.poster_url;
      img.alt = "";
      img.loading = "lazy";
      img.referrerPolicy = "no-referrer";
      img.addEventListener("error", function () {
        var fallback = placeholder(movie.title);
        if (img.parentNode) img.parentNode.replaceChild(fallback, img);
      });
      return img;
    }

    function placeholder(title) {
      var blank = document.createElement("div");
      blank.className = "poster poster-empty ph-" + (title.length % 6);
      blank.setAttribute("aria-hidden", "true");
      blank.appendChild(document.createTextNode(initials(title)));
      return blank;
    }

    function initials(text) {
      return text
        .split(/\s+/)
        .slice(0, 2)
        .map(function (word) {
          return word.charAt(0).toUpperCase();
        })
        .join("");
    }
  }

  /* ----------------------------------------------------- demo quick login */

  // In demo mode the login page lists the accounts; clicking one fills the
  // form so trying the app takes one click and no typing.
  document.querySelectorAll("button.demologin").forEach(function (choice) {
    choice.addEventListener("click", function () {
      var username = document.getElementById("username");
      var password = document.getElementById("password");
      if (!username || !password) return;

      username.value = choice.dataset.username || "";
      password.value = choice.dataset.password || "";
      var form = username.closest("form");
      if (form) form.submit();
    });
  });

  /* ------------------------------------------------------- delete confirm */

  // Two clicks instead of a modal: the first click arms the button, the second
  // submits. Reverts after a few seconds if the user walks away from it.
  document.querySelectorAll("form[data-confirm]").forEach(function (target) {
    var trigger = target.querySelector("button[type=submit]");
    if (!trigger) return;

    var original = trigger.textContent;
    var armed = false;
    var timer = null;

    trigger.addEventListener("click", function (event) {
      if (armed) return; // let the submit through

      event.preventDefault();
      armed = true;
      trigger.textContent = "Säker?";
      trigger.title = target.dataset.confirm;
      timer = window.setTimeout(function () {
        armed = false;
        trigger.textContent = original;
      }, 4000);
    });

    target.addEventListener("submit", function () {
      window.clearTimeout(timer);
    });
  });
})();
