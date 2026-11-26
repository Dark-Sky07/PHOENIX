/*global navigator*/
'use strict';

const {
  REGEX_BACKSLASH,
  REGEX_SPECIAL_CHARS,
  REGEX_SPECIAL_CHARS_GLOBAL
} = require('./constants');

exports.isObject = val => val !== null && typeof val === 'object' && !Array.isArray(val);
exports.hasRegexChars = str => REGEX_SPECIAL_CHARS.test(str);
exports.isRegexChar = str => str.length === 1 && exports.hasRegexChars(str);
exports.escapeRegex = str => str.replace(REGEX_SPECIAL_CHARS_GLOBAL, '\\$1');
exports.toPosixSlashes = str => str.replace(REGEX_BACKSLASH, '/');

exports.isWindows = () => {
  if (typeof navigator !== 'undefined' && navigator.platform) {
    const platform = navigator.platform.toLowerCase();
    return platform === 'win32' || platform === 'windows';
  }

  if (typeof process !== 'undefined' && process.platform) {
    return process.platform === 'win32';
  }

  return false;
};

// Equivalent to the former bracket-or-backslash replacement without a
// potentially quadratic search for a closing bracket at every opening bracket.
// A bracket match needs a non-backslash character before the closing bracket;
// as with JS regexp dot, line terminators may occur only as that final char.
exports.removeBackslashes = str => {
  const isLineTerminator = char => char === '\n' || char === '\r' || char === '\u2028' || char === '\u2029';
  const nextClose = new Int32Array(str.length + 1);
  nextClose[str.length] = -1;
  let close = -1;
  for (let i = str.length - 1; i >= 0; i--) {
    if (isLineTerminator(str[i])) {
      close = str[i + 1] === ']' ? i + 1 : -1;
    } else if (str[i] === ']' && i > 0 && str[i - 1] !== '\\') {
      close = i;
    }
    nextClose[i] = close;
  }

  const parts = [];
  let start = 0;
  for (let i = 0; i < str.length;) {
    if (str[i] === '[' && i + 2 < str.length && nextClose[i + 2] !== -1
      && (!isLineTerminator(str[i + 1]) || str[i + 2] === ']')) {
      i = nextClose[i + 2] + 1;
    } else if (str[i] === '\\' && i + 1 < str.length && !isLineTerminator(str[i + 1])) {
      parts.push(str.slice(start, i));
      start = ++i;
    } else {
      i++;
    }
  }
  parts.push(str.slice(start));
  return parts.join('');
};

exports.escapeLast = (input, char, lastIdx) => {
  const idx = input.lastIndexOf(char, lastIdx);
  if (idx === -1) return input;
  if (input[idx - 1] === '\\') return exports.escapeLast(input, char, idx - 1);
  return `${input.slice(0, idx)}\\${input.slice(idx)}`;
};

exports.removePrefix = (input, state = {}) => {
  let output = input;
  if (output.startsWith('./')) {
    output = output.slice(2);
    state.prefix = './';
  }
  return output;
};

exports.wrapOutput = (input, state = {}, options = {}) => {
  const prepend = options.contains ? '' : '^';
  const append = options.contains ? '' : '$';

  let output = `${prepend}(?:${input})${append}`;
  if (state.negated === true) {
    output = `(?:^(?!${output}).*$)`;
  }
  return output;
};

exports.basename = (path, { windows } = {}) => {
  const segs = path.split(windows ? /[\\/]/ : '/');
  const last = segs[segs.length - 1];

  if (last === '') {
    return segs[segs.length - 2];
  }

  return last;
};
