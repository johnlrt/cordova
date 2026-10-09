FROM node:24.12.0-alpine3.22 AS frontend
WORKDIR /build
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM nginx:1.28.0-alpine3.21
COPY infra/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=frontend /build/build/ /usr/share/nginx/html/
EXPOSE 8443
