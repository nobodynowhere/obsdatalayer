import js from '@eslint/js'
import pluginVue from 'eslint-plugin-vue'

export default [
  js.configs.recommended,
  ...pluginVue.configs['flat/recommended'],
  {
    files: ['**/*.vue', '**/*.js'],
    languageOptions: {
      ecmaVersion: 'latest',
      sourceType: 'module',
      globals: {
        // Browser globals
        window: 'readonly',
        document: 'readonly',
        console: 'readonly',
        localStorage: 'readonly',
        sessionStorage: 'readonly',
        btoa: 'readonly',
        atob: 'readonly',
        // Node globals (for build scripts)
        process: 'readonly',
        __dirname: 'readonly',
        __filename: 'readonly',
      },
    },
    rules: {
      // Vue specific rules
      'vue/multi-word-component-names': 'off',
      'vue/no-v-html': 'warn',
      'vue/require-default-prop': 'off',
      'vue/require-explicit-emits': 'off',
      'vue/max-attributes-per-line': 'off', // Allow flexible attribute formatting
      'vue/singleline-html-element-content-newline': 'off', // Allow single-line content
      'vue/html-self-closing': 'off', // Allow self-closing void elements
      'vue/attributes-order': 'off', // Allow flexible attribute ordering
      
      // General JavaScript rules
      'no-unused-vars': 'off', // Turned off as Vue handles this better
      'no-console': 'off',
      'no-debugger': 'warn',
      'prefer-const': 'warn',
    },
  },
]