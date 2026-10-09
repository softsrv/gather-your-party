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
};
