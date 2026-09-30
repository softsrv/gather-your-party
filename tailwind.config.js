/** @type {import('tailwindcss').Config} */
module.exports = {
  content: ["./internal/**/*.{go,js,templ,html}"],
  theme: {
    extend: {
      fontFamily: {
        sans: ["Inter", "ui-sans-serif", "system-ui", "sans-serif"],
        display: ['"Bricolage Grotesque"', "Inter", "ui-sans-serif", "sans-serif"],
      },
    },
  },
  plugins: [require("daisyui")],
  daisyui: {
    // "Tavern at night": deep indigo surfaces, warm amber primary, violet and mint accents.
    themes: [
      {
        tavern: {
          primary: "#f6b547",
          "primary-content": "#1f1403",
          secondary: "#a78bfa",
          "secondary-content": "#160c2e",
          accent: "#4fd1a5",
          "accent-content": "#04201a",
          neutral: "#2a2542",
          "neutral-content": "#e7e3f5",
          "base-100": "#1b1830",
          "base-200": "#151226",
          "base-300": "#0f0d1c",
          "base-content": "#e7e3f5",
          info: "#7cb4ff",
          success: "#4fd1a5",
          warning: "#f6b547",
          error: "#f47272",
          "--rounded-box": "1rem",
          "--rounded-btn": "0.7rem",
          "--rounded-badge": "999px",
          "--tab-radius": "0.7rem",
        },
      },
    ],
  },
};
