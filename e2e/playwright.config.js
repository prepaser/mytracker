const { defineConfig, devices } = require('@playwright/test');

module.exports = defineConfig({
  testDir: '.',
  testMatch: 'smoke.spec.js',
  workers: 1,
  use: {
    baseURL: 'http://127.0.0.1:38991',
    trace: 'retain-on-failure'
  },
  projects: [
    { name: 'desktop', use: { ...devices['Desktop Chrome'] } },
    { name: 'mobile', use: { ...devices['Pixel 7'] } }
  ],
  webServer: {
    command: 'sh run-server.sh',
    url: 'http://127.0.0.1:38991/healthz',
    reuseExistingServer: false,
    timeout: 30000
  }
});
