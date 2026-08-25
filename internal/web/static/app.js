// CineVote front-end: the movie search box and a two-step delete confirm.
// No dependencies, no build step.
(function () {
  "use strict";

  /* ---------------------------------------------------------- movie search */

  var form = document.getElementById("suggest-form");
  var button = document.getElementById("search-btn");
  var box = document.getElementById("search-results");

  if (form && button && box) {
    var title = document.getElementById("title");
    var year = document.getElementById("year");
    var posterURL = document.getElementById("poster_url");
    var overview = document.getElementById("overview");
    var sourceID = document.getElementById("source-id");
    var label = button.dataset.label || "filmdatabasen";
    var idle = button.textContent;

    // Picking a result stops being valid as soon as the title is edited by
    // hand, otherwise we would attach the wrong film's poster.
    title.addEventListener("input", function () {
      sourceID.value = "";
    });

    button.addEventListener("click", function () {
      var query = title.value.trim();
      if (!query) {
        title.focus();
        return;
      }

      button.disabled = true;
      button.textContent = "Söker…";
      message("Söker på " + label + "…");

      fetch("/api/search?q=" + encodeURIComponent(query), {
        headers: { Accept: "application/json" },
        credentials: "same-origin"
      })
        .then(function (resp) {
          return resp.json().then(function (body) {
            if (!resp.ok) {
              throw new Error(body.error || "Sökningen misslyckades.");
            }
            return body;
          });
        })
        .then(function (body) {
          render(body.results || []);
        })
        .catch(function (err) {
          message(err.message || "Sökningen misslyckades. Fyll i uppgifterna själv.");
        })
        .finally(function () {
          button.disabled = false;
          button.textContent = idle;
        });
    });

    function message(text) {
      box.hidden = false;
      box.textContent = "";
      var p = document.createElement("p");
      p.className = "results-msg";
      p.textContent = text;
      box.appendChild(p);
    }

    function render(results) {
      box.hidden = false;
      box.textContent = "";

      if (!results.length) {
        message("Inga träffar på " + label + ". Fyll i uppgifterna själv.");
        return;
      }

      results.forEach(function (movie) {
        var card = document.createElement("button");
        card.type = "button";
        card.className = "result";
        card.title = movie.title + (movie.year ? " (" + movie.year + ")" : "");

        if (movie.poster_url) {
          var img = document.createElement("img");
          img.className = "poster";
          img.src = movie.poster_url;
          img.alt = "Filmposter för " + movie.title;
          img.loading = "lazy";
          img.referrerPolicy = "no-referrer";
          card.appendChild(img);
        } else {
          var blank = document.createElement("div");
          blank.className = "poster poster-empty ph-" + (movie.title.length % 6);
          blank.appendChild(document.createTextNode(initials(movie.title)));
          card.appendChild(blank);
        }

        var wrap = document.createElement("span");
        wrap.className = "result-label";

        var name = document.createElement("span");
        name.className = "result-title";
        name.textContent = movie.title;
        wrap.appendChild(name);

        var sub = document.createElement("span");
        sub.className = "result-year";
        sub.textContent = [movie.year, movie.rating ? "★ " + movie.rating : ""]
          .filter(Boolean)
          .join(" · ");
        wrap.appendChild(sub);

        card.appendChild(wrap);
        card.addEventListener("click", function () {
          select(movie);
        });
        box.appendChild(card);
      });
    }

    // select fills the form from a search hit. source_id is what the server
    // re-resolves against OMDb, so the browser cannot invent metadata.
    function select(movie) {
      sourceID.value = movie.imdb_id || (movie.tmdb_id ? String(movie.tmdb_id) : "");
      title.value = movie.title;
      if (movie.year) year.value = movie.year;
      if (movie.poster_url) posterURL.value = movie.poster_url;
      if (movie.overview) overview.value = movie.overview;
      message("Valt: " + movie.title + (movie.year ? " (" + movie.year + ")" : "") + ".");
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
