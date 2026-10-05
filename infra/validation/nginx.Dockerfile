FROM nginx:1.28-alpine@sha256:a8b39bd9cf0f83869a2162827a0caf6137ddf759d50a171451b335cecc87d236

RUN apk add --no-cache curl openssl \
 && addgroup -g 10001 nmcp \
 && adduser -D -H -u 10001 -G nmcp nmcp
COPY infra/validation/nginx.conf /etc/nginx/nginx.conf
COPY infra/validation/nginx-entrypoint.sh /usr/local/bin/nmcp-nginx-entrypoint
ENTRYPOINT ["/usr/local/bin/nmcp-nginx-entrypoint"]
CMD ["nginx", "-g", "daemon off;"]
