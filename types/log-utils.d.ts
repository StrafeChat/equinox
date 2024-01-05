declare module 'ansi-colors' {
    interface Colors {
      gray: (text: string) => string;
      red: (text: string) => string;
      cyan: (text: string) => string;
      green: (text: string) => string;
      yellow: (text: string) => string;
      bold: {
        underline: (text: string) => string;
      };
      symbols: {
        check: string;
        cross: string;
        info: string;
        warning: string;
      };
    }
  
    const colors: Colors;
    export = colors;
  }
  
  declare module 'time-stamp' {
    const timestamp: (format: string) => string;
    export = timestamp;
  }
  
  declare const log: {
    error: string;
    info: string;
    success: string;
    warning: string;
    timestamp: string;
    ok: (str: string) => string;
    heading: (...args: any[]) => string;
  } & import('ansi-colors');
  
  export = log;