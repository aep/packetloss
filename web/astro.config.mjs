import { defineConfig } from 'astro/config';

// Fully static output. Data comes from the builder's JSON artifacts (DATA_DIR),
// never from a database — see PRODUCT.md.
export default defineConfig({
  output: 'static',
  site: 'https://packetloss.example',
  devToolbar: { enabled: false },
  // Landing page -> default country overview (the grid is the landing per PRODUCT.md).
  redirects: { '/': '/de/' },
});
